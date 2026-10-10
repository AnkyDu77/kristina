package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// runQueue — раннер без горутины: всё, что в очереди, по одной.
func runQueue(t *testing.T, b *bot) {
	t.Helper()
	for b.brainReady() {
		id, ok, err := b.store.claimTask()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return
		}
		b.tasks.run(context.Background(), id)
	}
}

func lastWith(sent []tgSent, sub string) tgSent {
	for i := len(sent) - 1; i >= 0; i-- {
		if strings.Contains(sent[i].text, sub) {
			return sent[i]
		}
	}
	return tgSent{}
}

func press(b *bot, s tgSent, data string) {
	b.onCallback(context.Background(), &tgCallback{
		ID: "q", From: &tgUser{ID: 1}, Data: data,
		Message: &tgMessage{MessageID: s.msgID, Chat: struct{ ID int64 }{100}, Text: s.text},
	})
}

func multiCall(calls ...[2]string) llmTurn {
	return func(*testing.T, llmRequest) map[string]any {
		var tc []any
		for i, c := range calls {
			tc = append(tc, map[string]any{"id": "c" + string(rune('a'+i)), "type": "function",
				"function": map[string]any{"name": c[0], "arguments": c[1]}})
		}
		return map[string]any{"content": "", "tool_calls": tc}
	}
}

func TestTaskFullCycle(t *testing.T) {
	fakeMCP(t)
	base, reqs := fakeLLM(t,
		// чат: модель решает, что это задача
		callTool("start_task", `{"goal":"сравнить две библиотеки sqlite для Go"}`),
		say("Взялась, пришлю итог."),
		// задача: план + поиск одним ходом
		func(t *testing.T, req llmRequest) map[string]any {
			if !strings.Contains(req.Messages[0].Content, "фоновую задачу") || req.Messages[1].Content != "Задача: сравнить две библиотеки sqlite для Go" {
				t.Errorf("задача должна стартовать со своим промптом и целью: %q", req.Messages[1].Content)
			}
			for _, tl := range req.Tools {
				if tl.Function.Name == "start_task" {
					t.Errorf("в задаче start_task не предлагаем")
				}
			}
			return multiCall(
				[2]string{"update_plan", `{"steps":[{"title":"Найти","status":"doing"},{"title":"Сравнить","status":"todo"}]}`},
				[2]string{"web_search", `{"objective":"sqlite go","search_queries":["go sqlite driver"]}`},
			)(t, req)
		},
		callTool("ask_user", `{"question":"Важнее CGO-free или скорость?","options":["CGO-free","скорость"]}`),
		func(t *testing.T, req llmRequest) map[string]any {
			if last := req.Messages[len(req.Messages)-1]; last.Content != "Владелец ответил: CGO-free" {
				t.Errorf("ответ владельца не дошёл: %q", last.Content)
			}
			return map[string]any{"content": "Итог: бери **modernc**."}
		},
	)
	b, sent := agentBot(t, base)

	b.handle(context.Background(), 100, &chatState{}, incoming{from: 1, text: "сравни библиотеки sqlite для Go"})
	card := lastWith(sent(), "📋 Задача #1")
	if card.msgID == 0 || !strings.Contains(card.text, "В очереди") {
		t.Fatalf("карточка: %+v", card)
	}

	runQueue(t, b)
	if st := b.store.taskStatus(1); st != "waiting" {
		t.Fatalf("задача должна ждать ответа, а она %s", st)
	}
	ask := lastWith(sent(), "спрашивает")
	if len(ask.buttons) != 2 || ask.buttons[0] != "ans:1:0" {
		t.Fatalf("вопрос: %+v", ask)
	}
	if c := lastWith(sent(), "📋 Задача #1"); !strings.Contains(c.text, "▶️ Найти") || !strings.Contains(c.text, "Жду твоего ответа") {
		t.Fatalf("карточка с планом и ожиданием: %q", c.text)
	}

	press(b, ask, "ans:1:0")
	if st := b.store.taskStatus(1); st != "queued" {
		t.Fatalf("после ответа — в очередь, а она %s", st)
	}
	press(b, ask, "ans:1:1") // второй ответ на тот же вопрос ничего не меняет

	runQueue(t, b)
	if len(reqs()) != 5 {
		t.Fatalf("ходов модели: %d", len(reqs()))
	}
	all := texts(sent())
	if !strings.Contains(strings.Join(all, "\n"), "Итог задачи #1") {
		t.Fatalf("итог не пришёл: %q", all)
	}
	if c := lastWith(sent(), "📋 Задача #1"); !strings.Contains(c.text, "✅ Готово") || !strings.Contains(c.text, "✅ Найти") {
		t.Fatalf("финальная карточка: %q", c.text)
	}
	tk, _ := b.store.getTask(1)
	if tk.Status != "done" || tk.Steps != 2 || tk.Result != "Итог: бери **modernc**." {
		t.Fatalf("задача: %+v", tk)
	}
	if h, _ := b.store.history(100, 10); len(h) != 4 || !strings.Contains(h[2].Content, "Фоновая задача #1") {
		t.Fatalf("итог в истории чата: %+v", h)
	}
}

func TestTaskApprovalSurvivesRestart(t *testing.T) {
	base, _ := fakeLLM(t,
		callTool("remember", `{"text":"пишет на Go"}`),
		func(t *testing.T, req llmRequest) map[string]any {
			if last := req.Messages[len(req.Messages)-1]; !strings.Contains(last.Content, "Запомнила как #1") {
				t.Errorf("результат подтверждённого вызова: %q", last.Content)
			}
			return map[string]any{"content": "Готово."}
		},
	)
	b, sent := agentBot(t, base)
	if err := b.taskCmd(context.Background(), 100, "запомни, что я пишу на Go"); err != nil {
		t.Fatal(err)
	}
	runQueue(t, b)
	if st := b.store.taskStatus(1); st != "waiting" {
		t.Fatalf("подтверждение паркует задачу, а она %s", st)
	}
	ask := lastWith(sent(), approvalPrefix)

	// «Рестарт»: новый процесс на той же базе
	if _, err := b.store.expirePending(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.store.requeueRunning(); err != nil {
		t.Fatal(err)
	}
	b2 := newBot(b.cfg, b.tg, b.llm, nil, nil, b.store)

	press(b2, ask, "act:1:ok")
	runQueue(t, b2)
	if st := b2.store.taskStatus(1); st != "done" {
		t.Fatalf("задача после рестарта и ✅: %s", st)
	}
	if mems, _ := b2.store.memories(100); len(mems) != 1 {
		t.Fatalf("память: %+v", mems)
	}
}

func TestTaskResumesAfterGPUSleep(t *testing.T) {
	fakeMCP(t)
	gpu := &gpuManager{state: "absent", idleTimeout: time.Minute}
	var llmBase string
	base, reqs := fakeLLM(t,
		func(t *testing.T, req llmRequest) map[string]any {
			// Пока модель думала, карту погасили
			gpu.mu.Lock()
			gpu.state = "absent"
			gpu.mu.Unlock()
			return callTool("web_search", `{"objective":"x","search_queries":["x"]}`)(t, req)
		},
		func(t *testing.T, req llmRequest) map[string]any {
			if last := req.Messages[len(req.Messages)-1]; last.Role != "tool" || !strings.Contains(last.Content, "go1.27.0") {
				t.Errorf("после сна задача должна продолжить с выдачей поиска: %+v", last)
			}
			return map[string]any{"content": "Нашла."}
		},
	)
	llmBase = base
	tg, sent := fakeTG(t)
	b := newBot(botConfig{allowed: map[int64]bool{1: true}, gpuAdmins: map[int64]bool{1: true}, historyTurns: 6},
		tg, newLLMClient("", "m"), nil, gpu, testStore(t))
	wake := func() {
		gpu.mu.Lock()
		gpu.state, gpu.endpoint = "running", llmBase
		gpu.mu.Unlock()
	}

	if err := b.taskCmd(context.Background(), 100, "найди x"); err != nil {
		t.Fatal(err)
	}
	card := lastWith(sent(), "📋 Задача #1")
	if !strings.Contains(card.text, "ждёт видеокарту") || card.buttons[0] != "wake" {
		t.Fatalf("спящая карта — карточка с кнопкой «разбудить»: %+v", card)
	}
	runQueue(t, b)
	if len(reqs()) != 0 {
		t.Fatal("ради задачи карту не будим")
	}

	wake()
	runQueue(t, b)
	if st := b.store.taskStatus(1); st != "queued" || len(reqs()) != 1 {
		t.Fatalf("уснула посреди задачи — обратно в очередь: %s, ходов %d", st, len(reqs()))
	}
	wake()
	runQueue(t, b)
	if st := b.store.taskStatus(1); st != "done" {
		t.Fatalf("после второго подъёма: %s", st)
	}
}

func TestTaskButtons(t *testing.T) {
	b, sent := agentBot(t, "http://unused")
	b.cfg.staticBase = ""
	b.gpu = &gpuManager{state: "absent", idleTimeout: time.Minute} // очередь не двигается
	if err := b.taskCmd(context.Background(), 100, "что-то"); err != nil {
		t.Fatal(err)
	}
	card := lastWith(sent(), "📋 Задача #1")
	for _, step := range []struct{ op, want string }{
		{"pause", "paused"}, {"pause", "paused"}, {"resume", "queued"},
		{"cancel", "cancelled"}, {"resume", "cancelled"}, {"retry", "queued"},
	} {
		press(b, card, "task:1:"+step.op)
		if st := b.store.taskStatus(1); st != step.want {
			t.Fatalf("%s → %s, ждали %s", step.op, st, step.want)
		}
	}
	// Чужая задача из другого чата кнопкой не управляется
	b.onCallback(context.Background(), &tgCallback{ID: "q", From: &tgUser{ID: 1}, Data: "task:1:cancel",
		Message: &tgMessage{MessageID: card.msgID, Chat: struct{ ID int64 }{999}}})
	if st := b.store.taskStatus(1); st != "queued" {
		t.Fatalf("кнопка из чужого чата сработала: %s", st)
	}
}

func TestCompactTranscript(t *testing.T) {
	big := strings.Repeat("я", 30000)
	tk := &task{Transcript: []chatMsg{
		{Role: "system", Content: "s"}, {Role: "user", Content: "u"},
		{Role: "tool", Content: big}, {Role: "tool", Content: big}, {Role: "tool", Content: big},
		{Role: "assistant", Content: "a"}, {Role: "tool", Content: big}, {Role: "assistant", Content: "b"}, {Role: "user", Content: "c"},
	}}
	compactTranscript(tk)
	if len([]rune(tk.Transcript[2].Content)) > 500 || len([]rune(tk.Transcript[3].Content)) > 500 {
		t.Fatal("старая выдача должна сократиться")
	}
	if tk.Transcript[6].Content != big {
		t.Fatal("свежую выдачу не трогаем")
	}
}
