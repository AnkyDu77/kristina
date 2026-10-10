package main

// Цикл агента: модель → инструменты → модель, пока она не ответит текстом
// или не кончатся шаги.
//
// Видеокарту занимаем на каждый ход модели отдельно, а не на весь ответ:
// пока идёт поиск или владелец думает над кнопкой «можно?», карта не
// «в работе», и watchdog честно считает простой. Если за это время её
// погасили, следующий ход вернёт errGPUAsleep, и вопрос уйдёт в inbox.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	toolTimeout = 90 * time.Second
	// Выдача инструментов за один ответ, рун. Дальше — отвечать тем, что
	// есть: иначе переполним контекст модели.
	maxToolBudget = 40000
	// approvalPrefix — начало вопроса «можно?»; по нему callback узнаёт
	// суть действия, чтобы переписать сообщение итогом.
	approvalPrefix = "🤚 Можно? "
)

// errBrainEmpty — модель «подумала», но ничего не сказала (весь ответ ушёл
// в рассуждения).
var errBrainEmpty = errors.New("модель не дала ответа — попробуй переформулировать")

// brain — адрес «мозга» и чем его освободить. Внешний «мозг» доступен
// всегда; свой — только пока работает карта. Будить её отсюда нельзя.
func (b *bot) brain() (string, func(), error) {
	if b.cfg.llmBase != "" {
		return b.cfg.llmBase, func() {}, nil
	}
	base, release, ok := b.acquireGPU()
	if !ok {
		return "", nil, errGPUAsleep
	}
	return base + "/llm/v1", release, nil
}

// brainReady — можно ли прямо сейчас думать, не дожидаясь подъёма карты.
func (b *bot) brainReady() bool {
	return b.cfg.llmBase != "" || b.gpu == nil || b.gpu.status().State == "running"
}

func (b *bot) think(ctx context.Context, msgs []chatMsg, tools []toolSpec) (chatMsg, error) {
	base, release, err := b.brain()
	if err != nil {
		return chatMsg{}, err
	}
	defer release()
	return b.llm.chat(ctx, base, msgs, tools)
}

// runAgent возвращает итоговый ответ и сколько инструментов он стоил:
// по этому числу решаем, была ли задача «крупной» и заслуживает ли кружка.
func (b *bot) runAgent(ctx context.Context, chatID int64, msgs []chatMsg) (answer string, steps int, err error) {
	prog := &progress{b: b, chatID: chatID}
	budget := maxToolBudget
	for turn := 0; ; turn++ {
		last := turn >= b.cfg.maxSteps || budget <= 0
		tools := b.chatSpecs
		if last {
			tools = nil
			if steps > 0 {
				// Роль user, а не system: шаблон чата Qwen не принимает
				// system посреди диалога
				msgs = append(msgs, chatMsg{Role: "user", Content: "(Шаги закончились. Ответь тем, что уже удалось узнать, и честно скажи, чего не успела.)"})
			}
		}
		out, err := b.think(ctx, msgs, tools)
		if err != nil {
			return "", steps, err
		}
		if len(out.ToolCalls) == 0 || last {
			if strings.TrimSpace(out.Content) == "" {
				return "", steps, errBrainEmpty
			}
			prog.done(ctx)
			return out.Content, steps, nil
		}

		msgs = append(msgs, out)
		for _, tc := range out.ToolCalls {
			steps++
			res := b.callTool(ctx, chatID, tc, prog)
			budget -= utf8.RuneCountInString(res)
			msgs = append(msgs, chatMsg{Role: "tool", ToolCallID: tc.ID, Content: res})
		}
		if ctx.Err() != nil {
			return "", steps, ctx.Err()
		}
	}
}

// callTool выполняет один вызов в чате. Ошибки не роняют ответ, а уходят
// модели текстом: пусть попробует иначе или честно скажет, что не вышло.
func (b *bot) callTool(ctx context.Context, chatID int64, tc toolCall, prog *progress) string {
	t, args, bad := b.lookupTool(tc)
	if t == nil {
		return bad
	}
	if t.status != nil {
		prog.add(ctx, truncRunes(t.status(args), 120))
	}

	var actionID int64
	if t.confirm != nil {
		summary, err := t.confirm(chatID, args)
		if err != nil {
			return "ошибка: " + err.Error()
		}
		ok, id, err := b.approve(ctx, chatID, tc.Function.Name, string(args), summary)
		if err != nil {
			return "ошибка: " + err.Error()
		}
		if !ok {
			return rejectedResult
		}
		actionID = id
	}
	return b.execTool(ctx, chatID, t, tc.Function.Name, args, actionID)
}

const rejectedResult = "Владелец не разрешил это действие (или не ответил). Не повторяй его без новой просьбы."

// lookupTool — инструмент и аргументы вызова; nil — вызов битый, и
// второе значение — что сказать модели.
func (b *bot) lookupTool(tc toolCall) (*tool, json.RawMessage, string) {
	name := tc.Function.Name
	t := b.tools[name]
	if t == nil {
		return nil, nil, "ошибка: инструмента " + name + " нет"
	}
	args := json.RawMessage(tc.Function.Arguments)
	if strings.TrimSpace(tc.Function.Arguments) == "" {
		args = json.RawMessage("{}")
	}
	if !json.Valid(args) {
		return nil, nil, "ошибка: аргументы — не JSON"
	}
	return t, args, ""
}

// execTool запускает инструмент (подтверждение, если нужно, уже получено)
// и пишет журнал и квитанцию.
func (b *bot) execTool(ctx context.Context, chatID int64, t *tool, name string, args json.RawMessage, actionID int64) string {
	start := time.Now()
	rctx, cancel := context.WithTimeout(ctx, toolTimeout)
	res, err := t.run(rctx, chatID, args)
	cancel()
	errText := ""
	if err != nil {
		errText = err.Error()
		res = "ошибка: " + errText
	}
	res = truncRunes(res, maxToolResult)
	if lerr := b.store.logTool(chatID, name, string(args), res, errText, time.Since(start)); lerr != nil {
		log.Printf("agent: журнал инструментов: %v", lerr)
	}
	if actionID != 0 {
		_ = b.store.finishAction(actionID, res)
	}
	return res
}

// approve спрашивает владельца кнопками и ждёт ответа. Решение принимает
// onCallback (кнопка) или таймаут — кто первый: decideAction атомарен.
func (b *bot) approve(ctx context.Context, chatID int64, tool, args, summary string) (ok bool, id int64, err error) {
	id, err = b.store.createAction(chatID, 0, tool, args, summary)
	if err != nil {
		return false, 0, err
	}
	ch := make(chan bool, 1)
	b.mu.Lock()
	b.waiting[id] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.waiting, id)
		b.mu.Unlock()
	}()

	msgID, err := b.tg.sendButtons(ctx, chatID, approvalPrefix+summary, approvalButtons(id))
	if err != nil {
		_, _ = b.store.decideAction(chatID, id, "expired")
		return false, id, err
	}

	timer := time.NewTimer(b.cfg.approvalTimeout)
	defer timer.Stop()
	select {
	case ok := <-ch:
		return ok, id, nil
	case <-ctx.Done():
		_, _ = b.store.decideAction(chatID, id, "expired")
		return false, id, ctx.Err()
	case <-timer.C:
	}
	won, err := b.store.decideAction(chatID, id, "expired")
	if err != nil {
		return false, id, err
	}
	if !won {
		// Кнопку нажали в ту же секунду — её решение уже в пути
		return <-ch, id, nil
	}
	_ = b.tg.editText(context.WithoutCancel(ctx), chatID, msgID, "⌛ Не дождалась ответа: "+summary, nil)
	return false, id, nil
}

func approvalButtons(actionID int64) [][]tgButton {
	return [][]tgButton{{
		{Text: "✅ Да", Data: fmt.Sprintf("act:%d:ok", actionID)},
		{Text: "❌ Нет", Data: fmt.Sprintf("act:%d:no", actionID)},
	}}
}

// progress — одно сообщение о ходе работы, которое дописывается по шагам:
// видно, что она делает, а чат не засыпан сообщениями.
type progress struct {
	b      *bot
	chatID int64
	msgID  int64
	lines  []string
}

func (p *progress) add(ctx context.Context, line string) {
	p.lines = append(p.lines, line)
	p.flush(ctx)
}

func (p *progress) done(ctx context.Context) {
	if p.msgID == 0 {
		return
	}
	p.lines = append(p.lines, "✍️ Отвечаю")
	p.flush(ctx)
}

func (p *progress) flush(ctx context.Context) {
	text := strings.Join(p.lines, "\n")
	if p.msgID == 0 {
		id, err := p.b.tg.sendButtons(ctx, p.chatID, text, nil)
		if err == nil {
			p.msgID = id
		}
		return
	}
	_ = p.b.tg.editText(ctx, p.chatID, p.msgID, text, nil)
}
