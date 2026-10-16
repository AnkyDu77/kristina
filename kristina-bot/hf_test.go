package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHF — CLI Higgsfield по сценарию: смета, создание, статусы.
type fakeHF struct {
	mu      sync.Mutex
	calls   [][]string
	status  map[string]string // id → статус у Higgsfield
	result  string
	created int
	cost    string
}

func newFakeHF(resultURL string) *fakeHF {
	return &fakeHF{status: map[string]string{}, result: resultURL, cost: "0.12"}
}

func (f *fakeHF) run(_ context.Context, jsonOut bool, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	switch {
	case args[0] == "generate" && args[1] == "cost":
		return []byte(`{"credits": ` + f.cost + `}`), nil
	case args[0] == "generate" && args[1] == "create":
		f.created++
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", f.created)
		f.status[id] = "in_progress"
		return json.Marshal([]string{id})
	case args[0] == "generate" && args[1] == "get":
		return json.Marshal(map[string]any{"id": args[2], "status": f.status[args[2]], "job_type": "x", "result_url": f.result})
	case args[0] == "account":
		return []byte(`{"credits": 50, "subscription_plan_type": "starter"}`), nil
	}
	return nil, fmt.Errorf("неожиданный вызов %v", args)
}

func (f *fakeHF) finishAll(status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id := range f.status {
		f.status[id] = status
	}
}

func (f *fakeHF) creates() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[1] == "create" {
			out = append(out, c)
		}
	}
	return out
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func hfBot(t *testing.T, llmBase string, f *fakeHF, limit, maxCall float64) (*bot, func() []tgSent) {
	t.Helper()
	b, sent := agentBot(t, llmBase)
	b.cfg.tz = time.UTC
	ws, err := openWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b.attach(ws, nil, nil)
	b.attachHiggsfield(newHiggsfield(f, "soul-kristina", limit, maxCall, 4))
	return b, sent
}

func TestHFRequestArgs(t *testing.T) {
	h := newHiggsfield(nil, "soul-1", 0, 0, 4)
	r := hfRequest{Model: "text2image_soul_v2", Prompt: "кадр", Params: map[string]string{"seed": "7", "aspect_ratio": "3:4"},
		Images: []string{"/tmp/a.png"}}
	h.prepare(&r)
	args, err := r.args("create")
	if err != nil {
		t.Fatal(err)
	}
	want := "generate create text2image_soul_v2 --prompt=кадр --aspect_ratio=3:4 --custom_reference_id=soul-1 --seed=7 --image=/tmp/a.png"
	if got := strings.Join(args, " "); got != want {
		t.Fatalf("аргументы:\n%s\nждали\n%s", got, want)
	}
	// Soul ID — только для Soul
	r2 := hfRequest{Model: "nano_banana_2_lite", Prompt: "x"}
	h.prepare(&r2)
	if _, ok := r2.Params["custom_reference_id"]; ok {
		t.Fatalf("Soul ID подставился не-Soul модели")
	}
	for _, bad := range []hfRequest{
		{Model: "kling; rm -rf /"},
		{Model: "kling_o3_flf", Params: map[string]string{"image": "x"}},
		{Model: "kling_o3_flf", Params: map[string]string{"--json": "x"}},
	} {
		if _, err := bad.args("create"); err == nil {
			t.Errorf("должны отказать: %+v", bad)
		}
	}
}

func TestHFErrLine(t *testing.T) {
	if got := hfErrLine("Error: No model with job_type \"x\".\nHint: Run: hf model get x\n"); got != `No model with job_type "x".` {
		t.Fatalf("got %q", got)
	}
}

func TestHFBudget(t *testing.T) {
	f := newFakeHF("")
	b, _ := hfBot(t, "http://unused", f, 10, 5)
	if err := b.hfBudget(6, 1); err == nil || !strings.Contains(err.Error(), "потолка") {
		t.Fatalf("потолок вызова: %v", err)
	}
	if _, err := b.store.addHFJob(&hfJob{JobID: "a", ChatID: 1, Model: "m", Cost: 8}); err != nil {
		t.Fatal(err)
	}
	if err := b.hfBudget(3, 1); err == nil || !strings.Contains(err.Error(), "лимит месяца") {
		t.Fatalf("лимит месяца: %v", err)
	}
	// Упавшую генерацию Higgsfield вернул — она лимит не съедает
	jobs, _ := b.store.pendingHFJobs()
	if _, err := b.store.finishHFJob(jobs[0].ID, "failed", "nsfw", "", "nsfw"); err != nil {
		t.Fatal(err)
	}
	if err := b.hfBudget(3, 1); err != nil {
		t.Fatalf("после возврата лимит свободен: %v", err)
	}
	for i := 0; i < 4; i++ {
		_, _ = b.store.addHFJob(&hfJob{JobID: fmt.Sprintf("p%d", i), ChatID: 1, Model: "m"})
	}
	if err := b.hfBudget(0.1, 1); err == nil || !strings.Contains(err.Error(), "одновременно") {
		t.Fatalf("слоты: %v", err)
	}
}

// Задача заказывает две генерации → кнопка владельца → hf_wait паркует →
// сторож забирает готовое, кладёт в workspace, шлёт фото и будит задачу →
// задача доводит кадр photo_finish.
func TestHFTaskFlow(t *testing.T) {
	img := pngBytes(t, 300, 400)
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(img) }))
	defer cdn.Close()
	f := newFakeHF(cdn.URL + "/hf_result.png")

	base, reqs := fakeLLM(t,
		callTool("hf_generate", `{"model":"text2image_soul_v2","prompt":"autumn park","params":{"aspect_ratio":"3:4"},"count":2,"name":"/workspace/content/t/raw/a"}`),
		func(t *testing.T, req llmRequest) map[string]any {
			if last := req.Messages[len(req.Messages)-1]; !strings.Contains(last.Content, "Поставила в Higgsfield") {
				t.Errorf("результат заказа: %q", last.Content)
			}
			return callTool("hf_wait", `{}`)(t, req)
		},
		func(t *testing.T, req llmRequest) map[string]any {
			last := req.Messages[len(req.Messages)-1]
			if !strings.Contains(last.Content, "/workspace/content/t/raw/a_1.png") || !strings.Contains(last.Content, "a_2.png") {
				t.Errorf("отчёт сторожа: %q", last.Content)
			}
			return callTool("photo_finish", `{"path":"content/t/raw/a_1.png","out":"content/t/final/01.jpg","align":"top","grain":0}`)(t, req)
		},
		func(t *testing.T, req llmRequest) map[string]any {
			if last := req.Messages[len(req.Messages)-1]; !strings.Contains(last.Content, "300×375") {
				t.Errorf("photo_finish: %q", last.Content)
			}
			return map[string]any{"content": "Готово: два кадра, первый доведён."}
		},
	)
	b, sent := hfBot(t, base, f, 10, 5)
	if err := b.taskCmd(context.Background(), 100, "сделай осеннее фото"); err != nil {
		t.Fatal(err)
	}
	runQueue(t, b)
	ask := lastWith(sent(), approvalPrefix)
	if !strings.Contains(ask.text, "×2 — 0.24 кр") || !strings.Contains(ask.text, "autumn park") {
		t.Fatalf("кнопка со сметой: %q", ask.text)
	}
	press(b, ask, "act:1:ok")
	runQueue(t, b)

	if st := b.store.taskStatus(1); st != "waiting" {
		t.Fatalf("hf_wait должен парковать задачу, а она %s", st)
	}
	if c := lastWith(sent(), "📋 Задача #1"); !strings.Contains(c.text, "Жду генерацию в Higgsfield") {
		t.Fatalf("карточка: %q", c.text)
	}
	cr := f.creates()
	if len(cr) != 2 {
		t.Fatalf("создано задач: %d", len(cr))
	}
	joined := strings.Join(cr[0], " ") + " | " + strings.Join(cr[1], " ")
	if strings.Count(joined, "--custom_reference_id=soul-kristina") != 2 || strings.Count(joined, "--seed=") != 2 {
		t.Fatalf("Soul ID и сиды копий: %s", joined)
	}

	b.hfPoll(context.Background()) // ещё в работе — ничего не происходит
	if st := b.store.taskStatus(1); st != "waiting" {
		t.Fatalf("рано разбудили: %s", st)
	}
	f.finishAll("completed")
	b.hfPoll(context.Background())
	if st := b.store.taskStatus(1); st != "queued" {
		t.Fatalf("всё готово — задача в очередь, а она %s", st)
	}
	photos := 0
	for _, s := range sent() {
		if s.method == "sendPhoto" && strings.Contains(s.text, "a_") {
			photos++
		}
	}
	if photos != 2 {
		t.Fatalf("превью владельцу: %d", photos)
	}
	if _, data, err := b.ws.readAll("content/t/raw/a_2.png", 1<<20); err != nil || !bytes.Equal(data, img) {
		t.Fatalf("результат в workspace: %v", err)
	}

	runQueue(t, b)
	if tk, _ := b.store.getTask(1); tk.Status != "done" {
		t.Fatalf("задача: %s %s", tk.Status, tk.Error)
	}
	if _, data, err := b.ws.readAll("content/t/final/01.jpg", 1<<20); err != nil || len(data) == 0 {
		t.Fatalf("финал photo_finish: %v", err)
	}
	if len(reqs()) != 4 {
		t.Fatalf("ходов модели: %d", len(reqs()))
	}
	if got := b.store.hfSpent(monthStart(time.Now().UTC())); got < 0.239 || got > 0.241 {
		t.Fatalf("траты: %v", got)
	}
}

func TestHFFailedJobNotifiesAndRefunds(t *testing.T) {
	f := newFakeHF("")
	b, sent := hfBot(t, "http://unused", f, 10, 5)
	f.status["00000000-0000-0000-0000-000000000009"] = "nsfw"
	if _, err := b.store.addHFJob(&hfJob{JobID: "00000000-0000-0000-0000-000000000009", ChatID: 100, Model: "nano_banana_2_lite", Name: "x/y", Cost: 1}); err != nil {
		t.Fatal(err)
	}
	b.hfPoll(context.Background())
	if m := lastWith(sent(), "nsfw"); !strings.Contains(m.text, "кредиты возвращены") {
		t.Fatalf("сообщение о неудаче: %+v", texts(sent()))
	}
	if got := b.store.hfSpent(time.Unix(0, 0)); got != 0 {
		t.Fatalf("упавшее не тратится: %v", got)
	}
}

func TestFinishPhoto(t *testing.T) {
	out, size, err := finishPhoto(pngBytes(t, 1536, 2048), photoOpts{aspectW: 4, aspectH: 5, align: "top", width: 1080, grain: 4})
	if err != nil {
		t.Fatal(err)
	}
	if size != image.Pt(1080, 1350) {
		t.Fatalf("размер: %v", size)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || format != "jpeg" || cfg.Width != 1080 || cfg.Height != 1350 {
		t.Fatalf("jpeg: %v %s %+v", err, format, cfg)
	}
	if _, _, err := parseAspect("4:0"); err == nil {
		t.Fatal("4:0 — не пропорция")
	}
}

func TestSkills(t *testing.T) {
	idx := skillsIndex()
	for _, name := range []string{"instagram —", "instagram/photos —", "instagram/reels —", "instagram/costs —"} {
		if !strings.Contains(idx, name) {
			t.Errorf("в оглавлении нет %q:\n%s", name, idx)
		}
	}
	for _, s := range listSkills() {
		text, err := readSkill(s.Name)
		if err != nil {
			t.Fatal(err)
		}
		if n := len([]rune(text)); n > maxToolResult {
			t.Errorf("%s: %d символов — не влезет в один ответ инструмента (%d)", s.Name, n, maxToolResult)
		}
		if s.Summary == "" {
			t.Errorf("%s: нет строки «> о чём»", s.Name)
		}
	}
	if _, err := readSkill("instagram/../../go.mod"); err == nil {
		t.Fatal("выход из skills/ должен быть закрыт")
	}
}
