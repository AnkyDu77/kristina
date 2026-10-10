package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSandbox запоминает команды и отвечает заготовкой.
type fakeSandbox struct {
	mu   sync.Mutex
	cmds []string
	out  string
	code int
}

func (f *fakeSandbox) run(_ context.Context, cmd string, _ time.Duration) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, cmd)
	return f.out, f.code, nil
}
func (f *fakeSandbox) status(context.Context) string { return "running" }
func (f *fakeSandbox) reset(context.Context) error   { return nil }

func testWorkspace(t *testing.T) *workspace {
	t.Helper()
	ws, err := openWorkspace(filepath.Join(t.TempDir(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.root.Close() })
	return ws
}

func TestWorkspaceStaysInside(t *testing.T) {
	ws := testWorkspace(t)
	secret := filepath.Join(filepath.Dir(ws.dir), "secret.env")
	if err := os.WriteFile(secret, []byte("TOKEN=123"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Песочница может насоздавать симлинков куда угодно
	if err := os.Symlink(secret, filepath.Join(ws.dir, "link.env")); err != nil {
		t.Fatal(err)
	}
	if out, err := ws.read("link.env", 1); err == nil {
		t.Fatalf("симлинк наружу прочитался: %q", out)
	}
	if _, _, err := ws.readAll("link.env", 1<<20); err == nil {
		t.Fatal("симлинк наружу отправился бы владельцу… и не только")
	}
	// «..» не выводит наружу: путь чистится внутрь workspace
	if out, err := ws.read("../secret.env", 1); err == nil {
		t.Fatalf("../ прочитался: %q", out)
	}
	if _, err := ws.write("../../escape.txt", "x", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws.dir, "escape.txt")); err != nil {
		t.Fatal("../../escape.txt должен лечь внутрь workspace")
	}
}

func TestWorkspaceReadWriteList(t *testing.T) {
	ws := testWorkspace(t)
	if _, err := ws.write("/workspace/src/main.go", "package main\n\nfunc main() {}\n", false); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.write("src/main.go", "// хвост\n", true); err != nil {
		t.Fatal(err)
	}
	out, err := ws.read("src/main.go", 3)
	if err != nil || !strings.Contains(out, "func main() {}\n// хвост") || !strings.Contains(out, "с 3-й") {
		t.Fatalf("%q, %v", out, err)
	}
	if err := os.WriteFile(filepath.Join(ws.dir, "a.bin"), []byte{0x7f, 'E', 'L', 'F', 0, 1}, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _ := ws.read("a.bin", 1); !strings.Contains(out, "двоичный") {
		t.Fatalf("двоичный файл: %q", out)
	}
	list, err := ws.list("")
	if err != nil || !strings.Contains(list, "src/main.go") || !strings.Contains(list, "inbox/") {
		t.Fatalf("%q, %v", list, err)
	}
	p, err := ws.saveUpload("../отчёт 2026.pdf", []byte("1"))
	p2, _ := ws.saveUpload("отчёт 2026.pdf", []byte("2"))
	if err != nil || p != "inbox/отчёт 2026.pdf" || p2 != "inbox/отчёт 2026-1.pdf" {
		t.Fatalf("загрузки: %q %q %v", p, p2, err)
	}
}

func TestUploadWithoutCaptionDoesNotThink(t *testing.T) {
	base, reqs := fakeLLM(t) // ни одного хода: модель звать нельзя
	b, sent := agentBot(t, base)
	b.attach(testWorkspace(t), nil, nil)

	b.handle(context.Background(), 100, &chatState{}, incoming{from: 1, file: &tgFile{FileID: "f1", Name: "notes.txt", Size: 10}})
	if len(reqs()) != 0 {
		t.Fatal("файл без подписи — модель не нужна")
	}
	if got := texts(sent()); len(got) != 1 || !strings.Contains(got[0], "inbox/notes.txt") {
		t.Fatalf("ответ: %q", got)
	}
	data, err := os.ReadFile(filepath.Join(b.ws.dir, "inbox", "notes.txt"))
	if err != nil || string(data) != "содержимое docs/f1" {
		t.Fatalf("файл: %q, %v", data, err)
	}
}

func TestUploadWithCaptionGoesToAgent(t *testing.T) {
	base, reqs := fakeLLM(t, func(t *testing.T, req llmRequest) map[string]any {
		q := req.Messages[len(req.Messages)-1].Content
		if !strings.Contains(q, "/workspace/inbox/report.txt") || !strings.Contains(q, "что тут?") {
			t.Errorf("модель должна узнать о файле и просьбе: %q", q)
		}
		return map[string]any{"content": "Отчёт о продажах."}
	})
	b, _ := agentBot(t, base)
	b.attach(testWorkspace(t), &fakeSandbox{}, nil)

	b.handle(context.Background(), 100, &chatState{}, incoming{from: 1, text: "что тут?", file: &tgFile{FileID: "f2", Name: "report.txt"}})
	if len(reqs()) != 1 {
		t.Fatalf("ходов: %d", len(reqs()))
	}
	names := map[string]bool{}
	for _, s := range b.specs(inChat) {
		names[s.Function.Name] = true
	}
	for _, n := range []string{"read_file", "write_file", "send_file", "list_files", "run_command"} {
		if !names[n] {
			t.Errorf("нет инструмента %s", n)
		}
	}
}

func TestRunCommandAndSendFile(t *testing.T) {
	sb := &fakeSandbox{out: strings.Repeat("строка\n", 3000) + "PASS", code: 0}
	base, _ := fakeLLM(t,
		callTool("run_command", `{"command":"go test ./..."}`),
		func(t *testing.T, req llmRequest) map[string]any {
			res := req.Messages[len(req.Messages)-1].Content
			if !strings.HasPrefix(res, "$ go test ./...\n…(начало обрезано)") || !strings.HasSuffix(res, "PASS\n[готово, код 0]") {
				t.Errorf("вывод команды: голова %q … хвост %q", res[:60], res[len(res)-40:])
			}
			return callTool("send_file", `{"path":"out.txt","caption":"держи"}`)(t, req)
		},
		say("Готово."),
	)
	b, sent := agentBot(t, base)
	b.attach(testWorkspace(t), sb, nil)
	if _, err := b.ws.write("out.txt", "результат", false); err != nil {
		t.Fatal(err)
	}

	b.handle(context.Background(), 100, &chatState{}, incoming{from: 1, text: "прогони тесты"})
	if len(sb.cmds) != 1 || sb.cmds[0] != "go test ./..." {
		t.Fatalf("команды: %q", sb.cmds)
	}
	if doc := lastWith(sent(), "|out.txt"); doc.method != "sendDocument" || !strings.HasPrefix(doc.text, "держи") {
		t.Fatalf("документ: %+v", doc)
	}
	// Без сети — без подтверждений; с сетью — каждая команда с ✅
	if b.toolByName("run_command").confirm != nil {
		t.Fatal("в песочнице без сети подтверждать нечего")
	}
	b.cfg.sandboxNetwork = "bridge"
	b.registerTools(b.computerTools())
	if b.toolByName("run_command").confirm == nil {
		t.Fatal("песочница с сетью — команды с подтверждением")
	}
}

func TestCappedBuffer(t *testing.T) {
	var c cappedBuffer
	chunk := strings.Repeat("x", 100<<10)
	for i := 0; i < 5; i++ {
		_, _ = c.Write([]byte(chunk))
	}
	if len(c.buf) != sandboxMaxOutput || !strings.Contains(c.String(), "отброшено") {
		t.Fatalf("буфер %d", len(c.buf))
	}
}

// fakeBrowser — Playwright MCP: tools/list и tools/call с текстом и картинкой.
func fakeBrowser(t *testing.T, key string) (url string, calls func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		reply := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		}
		schema := map[string]any{"type": "object", "properties": map[string]any{}}
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "b1")
			reply(map[string]any{})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			var tools []any
			for _, n := range []string{"browser_navigate", "browser_snapshot", "browser_type", "browser_evaluate", "browser_run_code_unsafe"} {
				tools = append(tools, map[string]any{"name": n, "description": n, "inputSchema": schema})
			}
			reply(map[string]any{"tools": tools})
		case "tools/call":
			mu.Lock()
			got = append(got, req.Params.Name)
			mu.Unlock()
			reply(map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "- heading \"Go 1.27\" [ref=e1]"},
				map[string]any{"type": "image", "mimeType": "image/png", "data": "iVBORw0KGgo="},
			}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/browser/mcp", func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func TestBrowserToolsFollowGPUAndPolicy(t *testing.T) {
	burl, calls := fakeBrowser(t, "mk")
	base, _ := fakeLLM(t,
		callTool("browser_navigate", `{"url":"file:///etc/passwd"}`),
		callTool("browser_navigate", `{"url":"https://go.dev"}`),
		func(t *testing.T, req llmRequest) map[string]any {
			res := req.Messages[len(req.Messages)-1].Content
			if !strings.Contains(res, "Go 1.27") || !strings.Contains(res, "скриншот (1) отправлен") {
				t.Errorf("выдача браузера: %q", res)
			}
			return map[string]any{"content": "Открыла."}
		},
	)
	// Мозг внешний (думать можно и без карты), браузер — на карте
	gpu := &gpuManager{state: "absent", idleTimeout: time.Minute}
	tg, sent := fakeTG(t)
	b := newBot(botConfig{allowed: map[int64]bool{1: true}, historyTurns: 6, browserEnabled: true, llmBase: base + "/v1"},
		tg, newLLMClient("", "m"), newMediaClient("mk"), gpu, testStore(t))

	// Карта спит — браузера нет, и искать его не идём
	b.ensureBrowser(context.Background())
	if b.toolByName("browser_navigate") != nil {
		t.Fatal("браузер нашёлся при спящей карте")
	}

	gpu.mu.Lock()
	gpu.state, gpu.endpoint = "running", strings.TrimSuffix(burl, "/browser/mcp")
	gpu.mu.Unlock()
	b.handle(context.Background(), 100, &chatState{}, incoming{from: 1, text: "открой go.dev"})

	if got := calls(); len(got) != 1 || got[0] != "browser_navigate" {
		t.Fatalf("file:// не должен дойти до браузера: %q", got)
	}
	if b.toolByName("browser_evaluate") != nil || b.toolByName("browser_run_code_unsafe") != nil {
		t.Fatal("свой JS в браузере модели не даём")
	}
	if b.toolByName("browser_type").confirm == nil {
		t.Fatal("ввод текста — с подтверждением")
	}
	if photo := lastWith(sent(), "📸"); photo.method != "sendPhoto" {
		t.Fatalf("скриншот владельцу: %+v", photo)
	}

	gpu.mu.Lock()
	gpu.state = "absent"
	gpu.mu.Unlock()
	for _, s := range b.specs(inChat) {
		if strings.HasPrefix(s.Function.Name, "browser_") {
			t.Fatalf("карта уснула, а %s всё ещё предлагается", s.Function.Name)
		}
	}
}

func TestExternalMCPServers(t *testing.T) {
	if _, err := parseMCPServers("Bad Name=https://x"); err == nil {
		t.Error("кривое имя пропущено")
	}
	if _, err := parseMCPServers("docs=http://127.0.0.1:1/mcp,x=ftp://a"); err == nil {
		t.Error("ftp:// пропущен")
	}
	ss, err := parseMCPServers("docs=https://mcp.example.com/mcp")
	if err != nil || len(ss) != 1 || ss[0].name != "docs" {
		t.Fatalf("%+v, %v", ss, err)
	}
	b, _ := agentBot(t, "http://unused")
	yes := true
	ro := mcpTool{Name: "resolve-library-id"}
	ro.Annotations.ReadOnlyHint = &yes
	if tl := b.externalTool(ss[0], ro); tl.spec.Name != "docs_resolve-library-id" || tl.confirm != nil {
		t.Fatalf("read-only: %+v", tl.spec.Name)
	}
	if tl := b.externalTool(ss[0], mcpTool{Name: "delete.all"}); tl.spec.Name != "docs_delete_all" || tl.confirm == nil {
		t.Fatalf("без readOnlyHint — с подтверждением: %q", tl.spec.Name)
	}
}
