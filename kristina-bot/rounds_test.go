package main

import (
	"context"
	"strings"
	"testing"
)

// roundsBot — бот с рабочей папкой и раундом из двух шагов.
func roundsBot(t *testing.T, base string, roundSteps, checkin int) (*bot, func() []tgSent) {
	b, sent := agentBot(t, base)
	b.attach(testWorkspace(t), nil, nil)
	b.cfg.taskMaxSteps, b.cfg.taskCheckinRounds = roundSteps, checkin
	return b, sent
}

func noTools(t *testing.T, req llmRequest, what string) {
	if req.Tools != nil {
		t.Errorf("%s — без инструментов", what)
	}
}

const journal = "Сделано: каркас сайта\nПроверено: ничего\nОсталось: стили\nСледующий шаг: globals.css\nЗастряла: нет"

func TestTaskContinuesAfterRoundWithJournal(t *testing.T) {
	base, reqs := fakeLLM(t,
		callTool("list_files", `{}`),
		callTool("write_file", `{"path":"a.txt","content":"x"}`),
		func(t *testing.T, req llmRequest) map[string]any {
			noTools(t, req, "журнал")
			if last := req.Messages[len(req.Messages)-1].Content; !strings.Contains(last, "Раунд 1 закончен, шагов в нём: 2") {
				t.Errorf("просьба о журнале: %q", last)
			}
			return map[string]any{"content": journal}
		},
		func(t *testing.T, req llmRequest) map[string]any {
			// Второй раунд — с чистого листа: системный промпт + цель с журналом
			if len(req.Messages) != 2 || !strings.Contains(req.Messages[1].Content, "это раунд 2") ||
				!strings.Contains(req.Messages[1].Content, "Следующий шаг: globals.css") {
				t.Errorf("ход после раунда: %d сообщений, %q", len(req.Messages), req.Messages[len(req.Messages)-1].Content)
			}
			return map[string]any{"content": "Сайт готов и собран."}
		},
	)
	b, sent := roundsBot(t, base, 2, 0)
	if err := b.taskCmd(context.Background(), 100, "сделай сайт"); err != nil {
		t.Fatal(err)
	}
	runQueue(t, b)

	tk, _ := b.store.getTask(1)
	if tk.Status != "done" || tk.Round != 1 || tk.Steps != 2 || tk.Journal != journal {
		t.Fatalf("задача: статус %s, раунд %d, шагов %d, журнал %q", tk.Status, tk.Round, tk.Steps, tk.Journal)
	}
	if len(reqs()) != 4 || !strings.Contains(strings.Join(texts(sent()), "\n"), "Сайт готов и собран.") {
		t.Fatalf("ходов %d, отправлено %q", len(reqs()), texts(sent()))
	}
}

func TestRoundJournalCanFinishTask(t *testing.T) {
	base, _ := fakeLLM(t,
		callTool("write_file", `{"path":"a.txt","content":"x"}`),
		say("ГОТОВО: всё сделано и проверено."),
	)
	b, _ := roundsBot(t, base, 1, 0)
	_ = b.taskCmd(context.Background(), 100, "сделай")
	runQueue(t, b)
	if tk, _ := b.store.getTask(1); tk.Status != "done" || tk.Result != "всё сделано и проверено." {
		t.Fatalf("%s %q", tk.Status, tk.Result)
	}
}

func TestCheckinAfterRounds(t *testing.T) {
	base, reqs := fakeLLM(t,
		callTool("write_file", `{"path":"a.txt","content":"x"}`),
		say(journal),
		// «продолжай» с указанием реплаем → следующий раунд видит указание
		func(t *testing.T, req llmRequest) map[string]any {
			if last := req.Messages[len(req.Messages)-1].Content; last != "Владелец: и логотип добавь" {
				t.Errorf("указание владельца: %q", last)
			}
			return callTool("write_file", `{"path":"logo.svg","content":"<svg/>"}`)(t, req)
		},
		say(journal),
		// «подведи итог» → последний ход без инструментов
		func(t *testing.T, req llmRequest) map[string]any {
			noTools(t, req, "итог по просьбе владельца")
			return map[string]any{"content": "Итог: каркас и логотип."}
		},
	)
	b, sent := roundsBot(t, base, 1, 1)
	_ = b.taskCmd(context.Background(), 100, "сделай сайт")
	runQueue(t, b)

	ask := lastWith(sent(), "Продолжать?")
	if b.store.taskStatus(1) != "waiting" || len(ask.buttons) != 2 || !strings.Contains(ask.text, "Осталось: стили") {
		t.Fatalf("контрольная точка: %s, %+v", b.store.taskStatus(1), ask)
	}
	if !b.replyToTask(context.Background(), 100, ask.msgID, "и логотип добавь") {
		t.Fatal("реплай на контрольную точку должен дойти до задачи")
	}
	runQueue(t, b)

	ask = lastWith(sent(), "Продолжать?")
	press(b, ask, "ans:1:1") // 🏁 Подведи итог
	runQueue(t, b)
	if tk, _ := b.store.getTask(1); tk.Status != "done" || tk.Round != 2 || len(reqs()) != 5 {
		t.Fatalf("статус %s, раунд %d, ходов %d", tk.Status, tk.Round, len(reqs()))
	}
}

func TestStalledRoundAsksOwner(t *testing.T) {
	base, _ := fakeLLM(t,
		callTool("read_file", `{"path":"нет-такого.txt"}`),
		say("Сделано: ничего\nЗастряла: нет"), // сама не признаёт, но все вызовы — ошибки
	)
	b, sent := roundsBot(t, base, 1, 0) // контрольные точки выключены — буксовку ловим всё равно
	_ = b.taskCmd(context.Background(), 100, "прочти файл")
	runQueue(t, b)
	if ask := lastWith(sent(), "Продолжать?"); b.store.taskStatus(1) != "waiting" || !strings.Contains(ask.text, "буксует") {
		t.Fatalf("%s, %q", b.store.taskStatus(1), ask.text)
	}
}

func TestChatLimitOffersBackgroundTask(t *testing.T) {
	base, _ := fakeLLM(t,
		callTool("write_file", `{"path":"a.txt","content":"x"}`),
		say("Успела только a.txt."),
	)
	b, sent := roundsBot(t, base, 25, 0)
	b.cfg.maxSteps = 1
	b.handle(context.Background(), 100, &chatState{}, incoming{from: 1, text: "напиши сайт"})

	offer := lastWith(sent(), "Доделать фоновой задачей")
	if len(offer.buttons) != 1 || offer.buttons[0] != "cont" {
		t.Fatalf("предложение: %+v", offer)
	}
	press(b, offer, "cont")
	tk, err := b.store.getTask(1)
	if err != nil || !strings.Contains(tk.Goal, "напиши сайт") || !strings.Contains(tk.Goal, "Успела только a.txt.") {
		t.Fatalf("задача: %+v, %v", tk, err)
	}
	press(b, offer, "cont")
	if _, err := b.store.getTask(2); err == nil {
		t.Fatal("одна кнопка — одна задача")
	}
}

func TestBudgetsFollowContext(t *testing.T) {
	b, _ := agentBot(t, "http://unused")
	if b.chatToolBudget() != minChatToolBudget || b.taskBudget() != minTaskBudget {
		t.Fatalf("без контекста — нижние пороги: %d, %d", b.chatToolBudget(), b.taskBudget())
	}
	b.cfg.contextTokens = 130000
	if b.chatToolBudget() != 136500 || b.taskBudget() != 195000 {
		t.Fatalf("130k: %d, %d", b.chatToolBudget(), b.taskBudget())
	}
}
