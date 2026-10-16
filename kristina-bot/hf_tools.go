package main

// Инструменты Higgsfield для Кристины и сторож генераций (hfLoop).
// Как устроены деньги и ожидание — в шапке higgsfield.go.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	hfTick        = 15 * time.Second
	hfMaxCount    = 4
	hfMaxDownload = 300 << 20
	hfMaxMedia    = 50 << 20
	tgPhotoMax    = 10 << 20 // больше — телеграм фото не примет, шлём документом
)

var (
	hfImageExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true}
	hfVideoExt = map[string]bool{".mp4": true, ".mov": true, ".webm": true}
)

// hfToolArgs — аргументы hf_cost / hf_generate от модели.
type hfToolArgs struct {
	Model      string         `json:"model"`
	Prompt     string         `json:"prompt"`
	Params     map[string]any `json:"params"`
	Images     []string       `json:"images"`
	Video      string         `json:"video"`
	StartImage string         `json:"start_image"`
	EndImage   string         `json:"end_image"`
	Count      int            `json:"count"`
	Name       string         `json:"name"`
}

func hfArgsSchema(withName bool) map[string]any {
	props := map[string]any{
		"model":  str("job_type модели: text2image_soul_v2, nano_banana_2_lite, nano_banana_pro, kling_o3_flf, hf_mult_motion_control… (см. скилл instagram/costs)"),
		"prompt": str("промпт по-английски, подробный"),
		"params": map[string]any{"type": "object", "description": "прочие параметры модели как есть: aspect_ratio, seed, duration, mode, resolution, sound, multi_shots, quality… Soul ID подставляется сам"},
		"images": map[string]any{"type": "array", "items": str(""), "maxItems": 14,
			"description": "картинки-референсы: пути в /workspace или id прошлых задач Higgsfield"},
		"video":       str("видео-референс: путь в /workspace (для Genjutsu, kling_video_edit)"),
		"start_image": str("первый кадр видео: путь в /workspace или id задачи"),
		"end_image":   str("последний кадр видео: путь в /workspace или id задачи"),
		"count":       map[string]any{"type": "integer", "minimum": 1, "maximum": hfMaxCount, "description": "сколько вариантов (разные seed), по умолчанию 1"},
	}
	if withName {
		props["name"] = str("куда сохранить результат в /workspace, без расширения: content/2026-10-20-park/raw/back (копиям допишется _1, _2…)")
	}
	return object(props, "model")
}

// hfParse — запрос из аргументов модели: параметры к строкам, файлы из
// workspace — копиями во временную папку (CLI откроет путь сам, и
// симлинк, подложенный песочницей, увёл бы наружу что угодно — например,
// .env бота. Копия через os.Root наружу не выпустит). cleanup — убрать копии.
func (b *bot) hfParse(raw json.RawMessage) (req hfRequest, a hfToolArgs, cleanup func(), err error) {
	cleanup = func() {}
	if err = json.Unmarshal(raw, &a); err != nil {
		return req, a, cleanup, err
	}
	a.Model = strings.TrimSpace(a.Model)
	if a.Count <= 0 {
		a.Count = 1
	}
	if a.Count > hfMaxCount {
		return req, a, cleanup, fmt.Errorf("count не больше %d", hfMaxCount)
	}
	req = hfRequest{Model: a.Model, Prompt: strings.TrimSpace(a.Prompt), Params: map[string]string{}}
	for k, v := range a.Params {
		switch x := v.(type) {
		case string:
			req.Params[k] = x
		case float64:
			req.Params[k] = strconv.FormatFloat(x, 'f', -1, 64)
		case bool:
			req.Params[k] = strconv.FormatBool(x)
		case nil:
		default:
			return req, a, cleanup, fmt.Errorf("параметр %s: нужна строка, число или true/false", k)
		}
	}
	tmp, err := os.MkdirTemp("", "hf-media-")
	if err != nil {
		return req, a, cleanup, err
	}
	cleanup = func() { _ = os.RemoveAll(tmp) }
	n := 0
	media := func(ref string, exts map[string]bool) (string, error) {
		ref = strings.TrimSpace(ref)
		if ref == "" || hfIDRe.MatchString(ref) {
			return ref, nil
		}
		if b.ws == nil {
			return "", errors.New("рабочей папки нет — референс можно дать только id задачи Higgsfield")
		}
		ext := strings.ToLower(path.Ext(ref))
		if !exts[ext] {
			return "", fmt.Errorf("%s: такой файл референсом не годится (нужно %s)", ref, extList(exts))
		}
		name, data, err := b.ws.readAll(ref, hfMaxMedia)
		if err != nil {
			return "", err
		}
		n++
		p := filepath.Join(tmp, fmt.Sprintf("%02d_%s", n, unsafeName.ReplaceAllString(name, "_")))
		return p, os.WriteFile(p, data, 0o600)
	}
	for _, ref := range a.Images {
		p, err := media(ref, hfImageExt)
		if err != nil {
			return req, a, cleanup, err
		}
		if p != "" {
			req.Images = append(req.Images, p)
		}
	}
	if p, err := media(a.Video, hfVideoExt); err != nil {
		return req, a, cleanup, err
	} else if p != "" {
		req.Videos = []string{p}
	}
	if req.StartImage, err = media(a.StartImage, hfImageExt); err != nil {
		return req, a, cleanup, err
	}
	if req.EndImage, err = media(a.EndImage, hfImageExt); err != nil {
		return req, a, cleanup, err
	}
	b.hf.prepare(&req)
	return req, a, cleanup, nil
}

func extList(m map[string]bool) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return strings.Join(out, " ")
}

// hfBudget — можно ли потратить total прямо сейчас; ошибка — почему нет.
func (b *bot) hfBudget(total float64, count int) error {
	h := b.hf
	if h.maxCall > 0 && total > h.maxCall {
		return fmt.Errorf("это %s кр — больше потолка одного вызова (%s кр). Уменьши разрешение, длительность или count, или попроси владельца поднять KRISTINA_HF_MAX_CALL",
			fmtCr(total), fmtCr(h.maxCall))
	}
	spent := b.store.hfSpent(monthStart(b.nowLocal()))
	if h.limit > 0 && spent+total > h.limit {
		return fmt.Errorf("лимит месяца: потрачено %s из %s кр, этот вызов — ещё %s. Дальше — только если владелец поднимет KRISTINA_HF_MONTHLY_LIMIT",
			fmtCr(spent), fmtCr(h.limit), fmtCr(total))
	}
	if p := b.store.hfPendingCount(); p+count > h.slots {
		return fmt.Errorf("в работе уже %d генераций из %d возможных одновременно — дождись их (hf_wait)", p, h.slots)
	}
	return nil
}

func (b *bot) hfConfirm(_ int64, raw json.RawMessage) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req, a, cleanup, err := b.hfParse(raw)
	defer cleanup()
	if err != nil {
		return "", err
	}
	one, err := b.hf.cost(ctx, req)
	if err != nil {
		return "", err
	}
	total := one * float64(a.Count)
	if err := b.hfBudget(total, a.Count); err != nil {
		return "", err
	}
	spent := b.store.hfSpent(monthStart(b.nowLocal()))
	var sb strings.Builder
	fmt.Fprintf(&sb, "🎨 Higgsfield: %s", req.Model)
	if a.Count > 1 {
		fmt.Fprintf(&sb, " ×%d", a.Count)
	}
	fmt.Fprintf(&sb, " — %s кр\nЗа месяц: %s", fmtCr(total), fmtCr(spent))
	if b.hf.limit > 0 {
		fmt.Fprintf(&sb, " из %s", fmtCr(b.hf.limit))
	}
	sb.WriteString(" кр")
	if req.Prompt != "" {
		sb.WriteString("\nПромпт: " + truncRunes(req.Prompt, 350))
	}
	var refs []string
	for _, r := range append(append(a.Images, a.Video, a.StartImage), a.EndImage) {
		if r = strings.TrimSpace(r); r != "" {
			refs = append(refs, r)
		}
	}
	if len(refs) > 0 {
		sb.WriteString("\nРеференсы: " + truncRunes(strings.Join(refs, ", "), 300))
	}
	return sb.String(), nil
}

func (b *bot) hfGenerate(ctx context.Context, chatID int64, raw json.RawMessage) (string, error) {
	req, a, cleanup, err := b.hfParse(raw)
	defer cleanup()
	if err != nil {
		return "", err
	}
	one, err := b.hf.cost(ctx, req) // из кэша: только что считали для кнопки
	if err != nil {
		return "", err
	}
	// Пока владелец думал, могли потратить или занять слоты — проверяем снова
	if err := b.hfBudget(one*float64(a.Count), a.Count); err != nil {
		return "", err
	}
	var taskID int64
	if t := taskFrom(ctx); t != nil {
		taskID = t.ID
	}
	base := strings.TrimSuffix(strings.TrimSpace(a.Name), path.Ext(a.Name))
	if base != "" && b.ws != nil {
		rel, err := b.ws.rel(base)
		if err != nil || rel == "." {
			return "", fmt.Errorf("name %q: нужен путь к файлу внутри /workspace", a.Name)
		}
		base = rel
	}
	var made []string
	for i := 0; i < a.Count; i++ {
		r := req
		if a.Count > 1 {
			r.Params = map[string]string{}
			for k, v := range req.Params {
				r.Params[k] = v
			}
			if _, ok := r.Params["seed"]; !ok {
				r.Params["seed"] = strconv.FormatUint(uint64(randUint32()%1_000_000), 10)
			}
		}
		id, err := b.hf.create(ctx, r)
		if err != nil {
			if len(made) == 0 {
				return "", err
			}
			made = append(made, "ещё одна копия не создалась: "+err.Error())
			break
		}
		name := base
		if name != "" && a.Count > 1 {
			name = fmt.Sprintf("%s_%d", base, i+1)
		}
		if name == "" {
			name = fmt.Sprintf("hf/%s/%s_%s", b.nowLocal().Format("2006-01-02"), req.Model, id[:8])
		}
		if _, err := b.store.addHFJob(&hfJob{JobID: id, ChatID: chatID, TaskID: taskID, Model: req.Model,
			Prompt: req.Prompt, Name: name, Cost: one}); err != nil {
			return "", err
		}
		made = append(made, fmt.Sprintf("%s → %s.*", id, name))
	}
	next := "Готовое владелец увидит в чате сам."
	if taskID != 0 {
		next += " Чтобы посмотреть результат и продолжить — hf_wait."
	}
	return fmt.Sprintf("Поставила в Higgsfield (по %s кр):\n%s\n%s", fmtCr(one), strings.Join(made, "\n"), next), nil
}

func randUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint32(b[:])
}

// ── Ожидание ────────────────────────────────────────────────────────────────

// hfReport — что стало с генерациями задачи с прошлого hf_wait.
func (b *bot) hfReport(taskID int64) (string, error) {
	jobs, err := b.store.unreportedHFJobs(taskID)
	if err != nil {
		return "", err
	}
	if len(jobs) == 0 {
		return "Новых результатов нет.", nil
	}
	var sb strings.Builder
	var ids []int64
	sb.WriteString("Результаты Higgsfield (владелец их уже видел в чате):\n")
	for _, j := range jobs {
		ids = append(ids, j.ID)
		if j.Status == "done" {
			fmt.Fprintf(&sb, "✅ %s — /workspace/%s (id %s)\n", j.Model, j.File, j.JobID)
		} else {
			fmt.Fprintf(&sb, "❌ %s — %s (id %s)\n", j.Model, j.Error, j.JobID)
		}
	}
	return sb.String(), b.store.markHFReported(ids)
}

// parkHFWait — hf_wait в задаче: генерации ещё идут — задача ложится
// ждать (видеокарту не держит), иначе — результат сразу.
func (b *bot) parkHFWait(ctx context.Context, t *task, calls []toolCall) (parked bool, result string, err error) {
	if b.hf == nil {
		return false, "ошибка: Higgsfield не подключён", nil
	}
	if b.store.hfPendingForTask(t.ID) == 0 {
		res, err := b.hfReport(t.ID)
		return false, res, err
	}
	t.Pending, t.Note = &pendingCall{Calls: calls, Kind: "hf_wait"}, "⏳ Жду Higgsfield"
	if err := b.park(t); err != nil {
		return true, "", err
	}
	// Всё могло доделаться, пока парковались
	b.hfWake(ctx, t.ID)
	return true, "", nil
}

// hfWake будит задачу, если она ждёт генерации и ждать больше нечего.
func (b *bot) hfWake(ctx context.Context, taskID int64) {
	if b.store.hfPendingForTask(taskID) > 0 {
		return
	}
	t, err := b.store.getTask(taskID)
	if err != nil || t.Pending == nil || t.Pending.Kind != "hf_wait" || t.Pending.resolved() {
		return
	}
	res, err := b.hfReport(taskID)
	if err != nil {
		log.Printf("hf: отчёт задаче #%d: %v", taskID, err)
		return
	}
	t.Pending.Answer = res
	if ok, err := b.store.answerTask(taskID, t.Pending); err != nil || !ok {
		return
	}
	if fresh, err := b.store.getTask(taskID); err == nil {
		b.refreshCard(ctx, fresh)
	}
	b.tasks.poke()
}

// ── Сторож ──────────────────────────────────────────────────────────────────

func (b *bot) hfLoop(ctx context.Context) {
	if credits, plan, err := b.hf.balance(ctx); err != nil {
		log.Printf("higgsfield: не отвечает (%v) — нет входа? Положи credentials.json в data/higgsfield (README)", err)
	} else {
		log.Printf("higgsfield: баланс %s кр, тариф %s", fmtCr(credits), plan)
	}
	tick := time.NewTicker(hfTick)
	defer tick.Stop()
	for {
		b.hfPoll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (b *bot) hfPoll(ctx context.Context) {
	jobs, err := b.store.pendingHFJobs()
	if err != nil {
		log.Printf("hf: %v", err)
		return
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		b.hfCheck(ctx, j)
	}
}

func (b *bot) hfCheck(ctx context.Context, j *hfJob) {
	info, err := b.hf.get(ctx, j.JobID)
	if err != nil {
		if time.Since(j.Created) > hfJobMaxAge {
			b.hfFail(ctx, j, "", "не дождалась ответа Higgsfield: "+err.Error())
		} else {
			log.Printf("hf: %s: %v", j.JobID, err)
		}
		return
	}
	switch hfState(info.Status) {
	case "pending":
		if time.Since(j.Created) > hfJobMaxAge {
			b.hfFail(ctx, j, info.Status, "Higgsfield так и не доделал задачу")
		}
		return
	case "failed":
		msg := "Higgsfield вернул «" + info.Status + "», кредиты он возвращает"
		if strings.EqualFold(info.Status, "nsfw") {
			msg = "фильтр Higgsfield счёл кадр nsfw (кредиты возвращены) — тот же запрос упадёт снова, меняй формулировку или кадр"
		}
		b.hfFail(ctx, j, info.Status, msg)
		return
	}
	data, ext, err := b.hfDownload(ctx, info)
	if err != nil {
		log.Printf("hf: %s: скачать: %v", j.JobID, err)
		return // попробуем на следующем круге; hfJobMaxAge не даст висеть вечно
	}
	file := j.Name + ext
	if b.ws != nil {
		if err := b.ws.writeBytes(file, data); err != nil {
			log.Printf("hf: %s: сохранить %s: %v", j.JobID, file, err)
			return
		}
	}
	if ok, err := b.store.finishHFJob(j.ID, "done", info.Status, file, ""); err != nil || !ok {
		return
	}
	caption := fmt.Sprintf("🎨 %s · %s", path.Base(j.Name), j.Model)
	if j.TaskID != 0 {
		caption += fmt.Sprintf(" (задача #%d)", j.TaskID)
	}
	if err := b.hfSendMedia(ctx, j.ChatID, path.Base(file), data, caption); err != nil {
		log.Printf("hf: %s: отправить: %v", j.JobID, err)
	}
	if j.TaskID != 0 {
		b.hfWake(ctx, j.TaskID)
	}
}

func (b *bot) hfFail(ctx context.Context, j *hfJob, hfStatus, msg string) {
	if ok, err := b.store.finishHFJob(j.ID, "failed", hfStatus, "", msg); err != nil || !ok {
		return
	}
	_ = b.tg.sendPlain(ctx, j.ChatID, fmt.Sprintf("❌ %s · %s: %s", path.Base(j.Name), j.Model, msg))
	if j.TaskID != 0 {
		b.hfWake(ctx, j.TaskID)
	}
}

// hfDownload — результат задачи. Сначала полный файл, не вышло (CDN бывает
// медленным и рвёт загрузку) — лёгкая копия того же разрешения.
func (b *bot) hfDownload(ctx context.Context, info hfJobInfo) ([]byte, string, error) {
	var errs []error
	for _, u := range []string{info.ResultURL, info.MinResultURL} {
		if u == "" {
			continue
		}
		data, err := httpGetLimited(ctx, u, hfMaxDownload, 10*time.Minute)
		if err == nil {
			return data, resultExt(u), nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil, "", errors.New("у готовой задачи нет ссылки на результат")
	}
	return nil, "", errors.Join(errs...)
}

func resultExt(u string) string {
	pu, err := url.Parse(u)
	if err != nil {
		return ".bin"
	}
	ext := strings.ToLower(path.Ext(pu.Path))
	if ext == "" || len(ext) > 5 {
		return ".bin"
	}
	return ext
}

func httpGetLimited(ctx context.Context, u string, limit int64, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", path.Base(u), resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s больше %s", path.Base(u), humanSize(limit))
	}
	return data, nil
}

// hfSendMedia — превью владельцу: фото, видео или документом, если велико.
func (b *bot) hfSendMedia(ctx context.Context, chatID int64, name string, data []byte, caption string) error {
	ext := strings.ToLower(path.Ext(name))
	switch {
	case hfImageExt[ext] && len(data) <= tgPhotoMax:
		return b.tg.sendPhotoNamed(ctx, chatID, name, data, caption)
	case hfVideoExt[ext] && len(data) <= maxSendBytes:
		return b.tg.sendVideoFile(ctx, chatID, name, data, caption)
	case len(data) <= maxSendBytes:
		return b.tg.sendDocument(ctx, chatID, name, data, caption)
	}
	return b.tg.sendPlain(ctx, chatID, caption+": файл больше 50 МБ, лежит в /workspace")
}

// ── Инструменты ────────────────────────────────────────────────────────────

func (b *bot) hfTools() []*tool {
	return []*tool{
		{
			spec: toolSpecFunc{
				Name:        "hf_cost",
				Description: "Смета генерации в Higgsfield (бесплатно, ничего не создаёт). Те же аргументы, что у hf_generate.",
				Parameters:  hfArgsSchema(false),
			},
			modes:   everywhere,
			timeout: 3 * time.Minute,
			status:  func(a json.RawMessage) string { return "🧮 Считаю смету: " + argStr(a, "model") },
			run: func(ctx context.Context, _ int64, raw json.RawMessage) (string, error) {
				req, a, cleanup, err := b.hfParse(raw)
				defer cleanup()
				if err != nil {
					return "", err
				}
				one, err := b.hf.cost(ctx, req)
				if err != nil {
					return "", err
				}
				res := fmt.Sprintf("%s: %s кр за штуку, ×%d = %s кр.", req.Model, fmtCr(one), a.Count, fmtCr(one*float64(a.Count)))
				if err := b.hfBudget(one*float64(a.Count), a.Count); err != nil {
					res += " Но сейчас не получится: " + err.Error()
				}
				return res, nil
			},
		},
		{
			spec: toolSpecFunc{
				Name: "hf_generate",
				Description: "Сгенерировать фото или видео в Higgsfield. Это деньги владельца: он подтверждает кнопкой со сметой, лимит месяца стоит в коде. " +
					"Задача создаётся и идёт сама 1–10 минут; готовое сохраняется в /workspace по name и приходит владельцу в чат. В фоновой задаче потом — hf_wait. " +
					"Перед первой генерацией прочти скилл instagram и нужный подскилл.",
				Parameters: hfArgsSchema(true),
			},
			modes:   everywhere,
			timeout: 5 * time.Minute,
			status:  func(a json.RawMessage) string { return "🎨 Заказываю в Higgsfield: " + argStr(a, "model") },
			confirm: b.hfConfirm,
			run:     b.hfGenerate,
		},
		{
			spec: toolSpecFunc{
				Name: "hf_wait",
				Description: "Дождаться заказанных генераций Higgsfield. Задача встанет на ожидание (видеокарту не держит) и продолжится, " +
					"когда всё готово: получишь пути к файлам в /workspace или причины неудач.",
				Parameters: object(map[string]any{}),
			},
			modes: everywhere,
			// В задаче вызов паркуется раньше (taskCalls); сюда — из чата
			run: func(ctx context.Context, chatID int64, _ json.RawMessage) (string, error) {
				return b.hfStatusText(ctx, chatID, false)
			},
		},
		{
			spec: toolSpecFunc{
				Name:        "hf_status",
				Description: "Higgsfield: баланс, траты за месяц и лимит, генерации в работе и последние результаты.",
				Parameters:  object(map[string]any{}),
			},
			modes:  everywhere,
			status: func(json.RawMessage) string { return "💳 Смотрю Higgsfield" },
			run: func(ctx context.Context, chatID int64, _ json.RawMessage) (string, error) {
				return b.hfStatusText(ctx, chatID, true)
			},
		},
		{
			spec: toolSpecFunc{
				Name:        "hf_model_info",
				Description: "Параметры модели Higgsfield: точные имена, допустимые значения, ограничения. Бесплатно.",
				Parameters:  object(map[string]any{"model": str("job_type модели")}, "model"),
			},
			modes:  everywhere,
			status: func(a json.RawMessage) string { return "📑 Смотрю модель " + argStr(a, "model") },
			run: func(ctx context.Context, _ int64, a json.RawMessage) (string, error) {
				return b.hf.modelInfo(ctx, strings.TrimSpace(argStr(a, "model")))
			},
		},
	}
}

func (b *bot) hfStatusText(ctx context.Context, chatID int64, withBalance bool) (string, error) {
	var sb strings.Builder
	if withBalance {
		if credits, plan, err := b.hf.balance(ctx); err == nil {
			fmt.Fprintf(&sb, "Баланс: %s кр (тариф %s).\n", fmtCr(credits), plan)
		} else {
			fmt.Fprintf(&sb, "Баланс узнать не вышло: %v\n", err)
		}
	}
	spent := b.store.hfSpent(monthStart(b.nowLocal()))
	fmt.Fprintf(&sb, "Потрачено за месяц: %s кр", fmtCr(spent))
	if b.hf.limit > 0 {
		fmt.Fprintf(&sb, " из %s (лимит)", fmtCr(b.hf.limit))
	}
	fmt.Fprintf(&sb, ". Потолок одного вызова: %s кр. В работе: %d из %d.\n", fmtCr(b.hf.maxCall), b.store.hfPendingCount(), b.hf.slots)
	jobs, err := b.store.recentHFJobs(chatID, 10)
	if err != nil {
		return "", err
	}
	if len(jobs) > 0 {
		sb.WriteString("Последние генерации:\n")
	}
	for _, j := range jobs {
		switch j.Status {
		case "done":
			fmt.Fprintf(&sb, "✅ %s — /workspace/%s\n", j.Model, j.File)
		case "failed":
			fmt.Fprintf(&sb, "❌ %s — %s\n", j.Model, truncRunes(j.Error, 120))
		default:
			fmt.Fprintf(&sb, "⏳ %s — идёт %s (%s)\n", j.Model, humanInterval(time.Since(j.Created).Round(time.Minute)), j.Name)
		}
	}
	return sb.String(), nil
}
