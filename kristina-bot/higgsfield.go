package main

// Higgsfield — генерация фото и видео для Instagram-профиля Кристины.
//
// Ходим через официальный CLI hf (Go-бинарь из @higgsfield/cli): он сам
// заливает локальные файлы-референсы и сам обновляет токен по refresh
// (~/.config/higgsfield/credentials.json — в контейнере это volume).
//
// Деньги. Каждая генерация — с кнопкой владельца; сверх того в коде —
// потолок одного вызова, лимит на месяц и число задач в работе (тариф
// Starter больше 4 сразу не берёт). Траты считаем по своим квитанциям в
// hf_jobs: неудачные задачи Higgsfield возвращает, их не считаем.
//
// Ожидание. Генерация идёт минутами, держать ради неё видеокарту с
// «мозгом» — выброшенные деньги. Задачу паркует hf_wait, а сторож hfLoop
// без LLM опрашивает Higgsfield, скачивает готовое в workspace, шлёт
// превью владельцу и будит задачу, когда всё её ждавшее готово.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	hfSoulModel = "text2image_soul_v2"
	// hfJobMaxAge — дольше этого задача у Higgsfield не живёт: бросаем ждать
	hfJobMaxAge = 3 * time.Hour
	hfCostTTL   = time.Hour
)

var (
	hfNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,48}$`)
	hfIDRe   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// параметры, которые собираем сами или которые ломают вызов
	hfReserved = map[string]bool{
		"prompt": true, "image": true, "images": true, "image_references": true,
		"video": true, "video_references": true, "start_image": true, "end_image": true,
		"wait": true, "wait_timeout": true, "wait_interval": true, "json": true, "no_color": true,
	}
)

// hfRunner — запуск CLI; в тестах подменяется.
type hfRunner interface {
	run(ctx context.Context, jsonOut bool, args ...string) ([]byte, error)
}

type hfCLI struct{ bin string }

func (c hfCLI) run(ctx context.Context, jsonOut bool, args ...string) ([]byte, error) {
	if jsonOut {
		args = append(args, "--json")
	}
	args = append(args, "--no-color")
	cmd := exec.CommandContext(ctx, c.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil || bytes.HasPrefix(bytes.TrimSpace(stdout.Bytes()), []byte("Error:")) {
		msg := hfErrLine(stderr.String())
		if msg == "" {
			msg = hfErrLine(stdout.String())
		}
		if msg == "" && err != nil {
			msg = err.Error()
		}
		return nil, errors.New("higgsfield: " + msg)
	}
	return stdout.Bytes(), nil
}

// hfErrLine — строка «Error: …» из вывода CLI (за ней бывает «Hint: …»).
func hfErrLine(s string) string {
	last := ""
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "Error:"); ok {
			return strings.TrimSpace(rest)
		}
		if line != "" {
			last = line
		}
	}
	return truncRunes(last, 300)
}

// hfRequest — одна генерация. Images/Videos/StartImage/EndImage — пути к
// локальным файлам (CLI зальёт их сам) или id загрузок/задач Higgsfield.
type hfRequest struct {
	Model                string
	Prompt               string
	Params               map[string]string
	Images, Videos       []string
	StartImage, EndImage string
}

func (r hfRequest) args(verb string) ([]string, error) {
	if !hfNameRe.MatchString(r.Model) {
		return nil, fmt.Errorf("странное имя модели %q", r.Model)
	}
	a := []string{"generate", verb, r.Model}
	if r.Prompt != "" {
		a = append(a, "--prompt="+r.Prompt)
	}
	keys := make([]string, 0, len(r.Params))
	for k := range r.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if hfReserved[k] || !hfNameRe.MatchString(k) {
			return nil, fmt.Errorf("параметр %q так не передаётся (картинки — images/start_image/end_image, видео — video)", k)
		}
		a = append(a, "--"+k+"="+r.Params[k])
	}
	for _, p := range r.Images {
		a = append(a, "--image="+p)
	}
	for _, p := range r.Videos {
		a = append(a, "--video="+p)
	}
	if r.StartImage != "" {
		a = append(a, "--start-image="+r.StartImage)
	}
	if r.EndImage != "" {
		a = append(a, "--end-image="+r.EndImage)
	}
	return a, nil
}

// key — отпечаток запроса для кэша смет (без случайного seed копий).
func (r hfRequest) key() string {
	raw, _ := json.Marshal(r)
	return string(raw)
}

type hfJobInfo struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	JobType      string `json:"job_type"`
	ResultURL    string `json:"result_url"`
	MinResultURL string `json:"min_result_url"`
}

// hfState — во что превращается статус Higgsfield: pending | done | failed.
func hfState(status string) string {
	switch strings.ToLower(status) {
	case "completed", "succeeded", "success", "done":
		return "done"
	case "failed", "nsfw", "canceled", "cancelled", "error", "rejected", "expired":
		return "failed"
	}
	return "pending"
}

type higgsfield struct {
	r       hfRunner
	soulID  string  // Soul ID Кристины — подставляется в Soul сам
	limit   float64 // кредитов в календарный месяц
	maxCall float64 // потолок одного hf_generate (все копии вместе)
	slots   int     // задач в работе одновременно

	mu    sync.Mutex
	costs map[string]hfCost
}

type hfCost struct {
	v  float64
	at time.Time
}

func newHiggsfield(r hfRunner, soulID string, limit, maxCall float64, slots int) *higgsfield {
	if slots <= 0 {
		slots = 4
	}
	return &higgsfield{r: r, soulID: soulID, limit: limit, maxCall: maxCall, slots: slots, costs: map[string]hfCost{}}
}

// prepare — то, что Кристина не должна помнить сама: свой Soul ID.
func (h *higgsfield) prepare(r *hfRequest) {
	if r.Params == nil {
		r.Params = map[string]string{}
	}
	if r.Model == hfSoulModel && r.Params["custom_reference_id"] == "" && h.soulID != "" {
		r.Params["custom_reference_id"] = h.soulID
	}
}

func (h *higgsfield) cost(ctx context.Context, r hfRequest) (float64, error) {
	h.mu.Lock()
	c, ok := h.costs[r.key()]
	h.mu.Unlock()
	if ok && time.Since(c.at) < hfCostTTL {
		return c.v, nil
	}
	args, err := r.args("cost")
	if err != nil {
		return 0, err
	}
	out, err := h.r.run(ctx, true, args...)
	if err != nil {
		return 0, err
	}
	var v struct {
		Credits *float64 `json:"credits"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.Credits == nil {
		return 0, fmt.Errorf("higgsfield: непонятная смета: %s", truncRunes(string(out), 200))
	}
	h.mu.Lock()
	h.costs[r.key()] = hfCost{*v.Credits, time.Now()}
	h.mu.Unlock()
	return *v.Credits, nil
}

func (h *higgsfield) create(ctx context.Context, r hfRequest) (string, error) {
	args, err := r.args("create")
	if err != nil {
		return "", err
	}
	out, err := h.r.run(ctx, true, args...)
	if err != nil {
		return "", err
	}
	var ids []string
	if err := json.Unmarshal(out, &ids); err != nil || len(ids) == 0 || !hfIDRe.MatchString(ids[0]) {
		return "", fmt.Errorf("higgsfield: задача не создалась: %s", truncRunes(string(out), 200))
	}
	return ids[0], nil
}

func (h *higgsfield) get(ctx context.Context, id string) (hfJobInfo, error) {
	var j hfJobInfo
	if !hfIDRe.MatchString(id) {
		return j, fmt.Errorf("странный id задачи %q", id)
	}
	out, err := h.r.run(ctx, true, "generate", "get", id)
	if err != nil {
		return j, err
	}
	if err := json.Unmarshal(out, &j); err != nil {
		return j, fmt.Errorf("higgsfield: непонятный ответ: %s", truncRunes(string(out), 200))
	}
	return j, nil
}

func (h *higgsfield) balance(ctx context.Context) (credits float64, plan string, err error) {
	out, err := h.r.run(ctx, true, "account", "status")
	if err != nil {
		return 0, "", err
	}
	var v struct {
		Credits float64 `json:"credits"`
		Plan    string  `json:"subscription_plan_type"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return 0, "", fmt.Errorf("higgsfield: непонятный баланс: %s", truncRunes(string(out), 200))
	}
	return v.Credits, v.Plan, nil
}

func (h *higgsfield) modelInfo(ctx context.Context, model string) (string, error) {
	if !hfNameRe.MatchString(model) {
		return "", fmt.Errorf("странное имя модели %q", model)
	}
	out, err := h.r.run(ctx, false, "model", "get", model)
	return string(out), err
}

// fmtCr — кредиты без лишних нулей: 0.12, 6.25, 35.
func fmtCr(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// monthStart — начало календарного месяца владельца: от него лимит.
func monthStart(now time.Time) time.Time {
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
}
