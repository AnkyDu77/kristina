package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// llmTurn — что фейковая модель отвечает на очередной запрос.
type llmTurn func(t *testing.T, req llmRequest) map[string]any

type llmRequest struct {
	Messages []chatMsg  `json:"messages"`
	Tools    []toolSpec `json:"tools"`
}

// fakeLLM — OpenAI-совместимый сервер по сценарию: i-й запрос — i-й ход.
func fakeLLM(t *testing.T, turns ...llmTurn) (base string, requests func() []llmRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []llmRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		var req llmRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		got = append(got, req)
		i := len(got) - 1
		mu.Unlock()
		if i >= len(turns) {
			t.Errorf("лишний запрос к модели #%d", i+1)
			http.Error(w, "no more turns", 500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": turns[i](t, req)}}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []llmRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]llmRequest(nil), got...)
	}
}

func say(text string) llmTurn {
	return func(*testing.T, llmRequest) map[string]any { return map[string]any{"content": text} }
}

func callTool(name, args string) llmTurn {
	return func(*testing.T, llmRequest) map[string]any {
		return map[string]any{"content": "", "tool_calls": []any{map[string]any{
			"id": "c1", "type": "function", "function": map[string]any{"name": name, "arguments": args},
		}}}
	}
}

// fakeMCP — Parallel Search MCP: initialize → сессия, tools/call → выдача.
func fakeMCP(t *testing.T) (calls func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			if r.Header.Get("Mcp-Session-Id") != "s1" {
				t.Errorf("tools/call без сессии")
			}
			mu.Lock()
			got = append(got, req.Params.Name)
			mu.Unlock()
			// SSE-ответ — сервер вправе выбрать и его
			w.Header().Set("Content-Type", "text/event-stream")
			result, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "{}"}},
				"structuredContent": map[string]any{"results": []any{map[string]any{
					"url": "https://go.dev/doc/devel/release", "title": "Release History",
					"excerpts": []string{"go1.27.0 (released 2026-08-19)"},
				}}},
			}})
			_, _ = w.Write([]byte("event: message\ndata: " + string(result) + "\n\n"))
		}
	}))
	t.Cleanup(srv.Close)
	old := parallelMCPURL
	parallelMCPURL = srv.URL
	t.Cleanup(func() { parallelMCPURL = old })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// agentBot — бот в статическом режиме: «мозг» на <base>/llm/v1.
func agentBot(t *testing.T, llmBase string) (*bot, func() []tgSent) {
	tg, sent := fakeTG(t)
	b := newBot(botConfig{
		allowed:         map[int64]bool{1: true},
		gpuAdmins:       map[int64]bool{1: true},
		historyTurns:    6,
		staticBase:      llmBase,
		approvalTimeout: 5 * time.Second,
	}, tg, newLLMClient("", "m"), nil, nil, testStore(t))
	return b, sent
}

func TestAgentSearchesThenAnswers(t *testing.T) {
	mcp := fakeMCP(t)
	base, reqs := fakeLLM(t,
		callTool("web_search", `{"objective":"последняя версия Go","search_queries":["go release"]}`),
		func(t *testing.T, req llmRequest) map[string]any {
			last := req.Messages[len(req.Messages)-1]
			if last.Role != "tool" || last.ToolCallID != "c1" || !strings.Contains(last.Content, "go1.27.0") {
				t.Errorf("модель не увидела выдачу поиска: %+v", last)
			}
			return map[string]any{"content": "<think>ну-ка</think>Свежая — **Go 1.27**."}
		},
	)
	b, sent := agentBot(t, base)

	b.handle(context.Background(), 100, &chatState{}, incoming{from: 1, text: "какая сейчас версия Go?"})

	if got := mcp(); len(got) != 1 || got[0] != "web_search" {
		t.Fatalf("вызовы MCP: %q", got)
	}
	r := reqs()
	if len(r[0].Tools) != len(b.toolSpecs) || !strings.Contains(r[0].Messages[0].Content, "Сегодня") {
		t.Fatalf("первый ход — с инструментами и датой в системном промпте")
	}
	all := strings.Join(texts(sent()), "\n---\n")
	if !strings.Contains(all, "🔎 Ищу: последняя версия Go") || !strings.Contains(all, "<b>Go 1.27</b>") || strings.Contains(all, "ну-ка") {
		t.Fatalf("отправлено:\n%s", all)
	}
	hist, _ := b.store.history(100, 10)
	if len(hist) != 2 || hist[1].Content != "Свежая — **Go 1.27**." {
		t.Fatalf("история: %+v", hist)
	}
	var runs int
	_ = b.store.db.QueryRow(`SELECT COUNT(*) FROM tool_runs`).Scan(&runs)
	if runs != 1 {
		t.Fatalf("журнал инструментов: %d", runs)
	}
}

func TestAgentAsksBeforeRemembering(t *testing.T) {
	for _, tc := range []struct {
		name   string
		button string
		want   int // записей в памяти после
	}{{"да", "ok", 1}, {"нет", "no", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			base, _ := fakeLLM(t,
				callTool("remember", `{"text":"владелец пишет на Go"}`),
				func(t *testing.T, req llmRequest) map[string]any {
					res := req.Messages[len(req.Messages)-1].Content
					if tc.want == 0 && !strings.Contains(res, "не разрешил") {
						t.Errorf("модель должна узнать об отказе: %q", res)
					}
					return map[string]any{"content": "Хорошо."}
				},
			)
			b, sent := agentBot(t, base)

			done := make(chan struct{})
			go func() {
				defer close(done)
				b.handle(context.Background(), 100, &chatState{}, incoming{from: 1, text: "я пишу на Go, запомни"})
			}()

			// Ждём вопрос «можно?» и жмём кнопку
			var ask tgSent
			for deadline := time.Now().Add(3 * time.Second); ask.msgID == 0; {
				for _, s := range sent() {
					if strings.HasPrefix(s.text, approvalPrefix) {
						ask = s
					}
				}
				if time.Now().After(deadline) {
					t.Fatalf("вопроса так и не было: %q", texts(sent()))
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !strings.Contains(ask.text, "владелец пишет на Go") || len(ask.buttons) != 2 {
				t.Fatalf("вопрос: %+v", ask)
			}
			b.onCallback(context.Background(), &tgCallback{
				ID: "q", From: &tgUser{ID: 1}, Data: "act:1:" + tc.button,
				Message: &tgMessage{MessageID: ask.msgID, Chat: struct{ ID int64 }{100}, Text: ask.text},
			})
			<-done

			mems, _ := b.store.memories(100)
			if len(mems) != tc.want {
				t.Fatalf("память: %+v", mems)
			}
			// Повторное нажатие — «уже неактуально», второй записи нет
			b.onCallback(context.Background(), &tgCallback{
				ID: "q2", From: &tgUser{ID: 1}, Data: "act:1:ok",
				Message: &tgMessage{MessageID: ask.msgID, Chat: struct{ ID int64 }{100}, Text: ask.text},
			})
			if mems, _ := b.store.memories(100); len(mems) != tc.want {
				t.Fatalf("второе нажатие что-то изменило: %+v", mems)
			}
		})
	}
}

func TestInboxAnsweredInOneRun(t *testing.T) {
	base, reqs := fakeLLM(t, say("Отвечаю на оба."))
	tg, sent := fakeTG(t)
	gpu := &gpuManager{state: "absent", idleTimeout: time.Minute}
	st := testStore(t)
	b := newBot(botConfig{allowed: map[int64]bool{1: true}, gpuAdmins: map[int64]bool{1: true}, historyTurns: 6},
		tg, newLLMClient("", "m"), nil, gpu, st)

	cs := &chatState{}
	b.handle(context.Background(), 100, cs, incoming{from: 1, text: "первый"})
	b.handle(context.Background(), 100, cs, incoming{from: 1, text: "второй"})
	if len(reqs()) != 0 {
		t.Fatal("спящая карта — к модели не ходим")
	}

	// Карта проснулась (gpu/llm/v1 — тот же фейк)
	gpu.mu.Lock()
	gpu.state, gpu.endpoint = "running", strings.TrimSuffix(base, "/")
	gpu.mu.Unlock()
	b.handle(context.Background(), 100, cs, incoming{inbox: true})

	r := reqs()
	if len(r) != 1 {
		t.Fatalf("ждали один прогон на оба вопроса, было %d", len(r))
	}
	q := r[0].Messages[len(r[0].Messages)-1].Content
	if !strings.Contains(q, "1. первый") || !strings.Contains(q, "2. второй") {
		t.Fatalf("вопрос модели: %q", q)
	}
	if st.countInbox(100) != 0 {
		t.Fatal("отложенное не закрыто")
	}
	if all := texts(sent()); all[len(all)-1] != "Отвечаю на оба." {
		t.Fatalf("отправлено: %q", all)
	}
	// Повторный сигнал — отвечать уже не на что
	b.handle(context.Background(), 100, cs, incoming{inbox: true})
	if len(reqs()) != 1 {
		t.Fatal("закрытое отложенное ответили второй раз")
	}
}

func TestAgentStopsAfterMaxSteps(t *testing.T) {
	loop := callTool("read_page", `{"url":"http://127.0.0.1/secret"}`)
	base, reqs := fakeLLM(t, loop, loop,
		func(t *testing.T, req llmRequest) map[string]any {
			if req.Tools != nil {
				t.Errorf("на последнем шаге инструменты не предлагаем")
			}
			return map[string]any{"content": "Не вышло прочесть."}
		},
	)
	mcp := fakeMCP(t)
	b, _ := agentBot(t, base)
	b.cfg.maxSteps = 2

	b.handle(context.Background(), 100, &chatState{}, incoming{from: 1, text: "прочти"})

	r := reqs()
	if len(r) != 3 {
		t.Fatalf("ходов модели: %d", len(r))
	}
	toolRes := r[1].Messages[len(r[1].Messages)-1].Content
	if !strings.Contains(toolRes, "не в публичном интернете") {
		t.Fatalf("localhost должен отбиваться: %q", toolRes)
	}
	if len(mcp()) != 0 {
		t.Fatal("внутренний адрес нельзя отдавать и внешнему сервису")
	}
}
