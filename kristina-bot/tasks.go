package main

// Фоновые задачи: долгая работа агента (исследовать, сравнить, собрать)
// вне очереди чата. В чате видна одна карточка задачи — план, статус,
// кнопки — и итог отдельным сообщением.
//
// Деньги. Задача ничего не держит, пока ждёт: вопрос владельцу или
// подтверждение паркуют её в базе (status = waiting, ход агента — в
// transcript), и раннер берётся за следующую. Видеокарту задача не
// будит: спит карта — задача ждёт в очереди с кнопкой «Разбудить»; карта
// уснула посреди задачи — та вернётся в очередь и продолжит с того же шага.
//
// Раннер один и задачи идут по одной: карта одна, а очередь задач —
// не повод держать её дольше.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// Сколько выдачи инструментов держим в ходе задачи, рун (~20k токенов
	// при контексте 32k). Старая выдача сверх этого — сокращается.
	taskContextBudget = 60000
	taskKeepRecent    = 4 // последние сообщения не сокращаем
)

type taskRunner struct {
	b    *bot
	kick chan struct{}

	mu      sync.Mutex
	running map[int64]context.CancelFunc
}

func newTaskRunner(b *bot) *taskRunner {
	return &taskRunner{b: b, kick: make(chan struct{}, 1), running: map[int64]context.CancelFunc{}}
}

// poke — «посмотри очередь»: новая задача, ответ владельца, проснулась карта.
func (r *taskRunner) poke() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

func (r *taskRunner) loop(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		// Без «мозга» очередь не трогаем: задачи дождутся подъёма карты
		for ctx.Err() == nil && r.b.brainReady() {
			id, ok, err := r.b.store.claimTask()
			if err != nil {
				log.Printf("tasks: очередь: %v", err)
				break
			}
			if !ok {
				break
			}
			r.run(ctx, id)
		}
		select {
		case <-ctx.Done():
			return
		case <-r.kick:
		case <-tick.C:
		}
	}
}

func (r *taskRunner) cancel(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.running[id]; c != nil {
		c()
	}
}

type taskCtxKey struct{}

// taskFrom — задача, в которой сейчас работает инструмент (nil — чат).
func taskFrom(ctx context.Context) *task {
	t, _ := ctx.Value(taskCtxKey{}).(*task)
	return t
}

func (r *taskRunner) run(ctx context.Context, id int64) {
	b := r.b
	t, err := b.store.getTask(id)
	if err != nil {
		log.Printf("tasks: #%d: %v", id, err)
		_, _ = b.store.finishTask(id, "failed", "", err.Error())
		return
	}
	tctx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.running[id] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.running, id)
		r.mu.Unlock()
		cancel()
	}()

	err = b.execTask(context.WithValue(tctx, taskCtxKey{}, t), t)
	switch {
	case err == nil:
		// закончена, припаркована или поставлена на паузу — всё уже записано
	case ctx.Err() != nil:
		// Бот останавливается: status остаётся running, и на старте
		// requeueRunning вернёт задачу в очередь с последнего шага
		_ = b.store.saveTaskProgress(t)
		return
	case tctx.Err() != nil:
		// отменили кнопкой — статус cancelled уже записан
		_ = b.store.saveTaskProgress(t)
	case errors.Is(err, errGPUAsleep):
		t.Note = "Видеокарта уснула — продолжу с этого места, когда проснётся"
		_ = b.store.saveTaskProgress(t)
		_, _ = b.store.setTaskStatus(id, "queued", "running")
	default:
		log.Printf("tasks: #%d: %v", id, err)
		t.Note = ""
		_ = b.store.saveTaskProgress(t)
		_, _ = b.store.finishTask(id, "failed", "", err.Error())
	}
	b.refreshCard(ctx, t)
}

func (b *bot) execTask(ctx context.Context, t *task) error {
	if len(t.Transcript) == 0 {
		mems, err := b.store.memories(t.ChatID)
		if err != nil {
			return err
		}
		t.Transcript = []chatMsg{
			{Role: "system", Content: systemPrompt(mems, time.Now().In(b.cfg.tz), true)},
			{Role: "user", Content: "Задача: " + t.Goal},
		}
	}

	if p := t.Pending; p != nil {
		if !p.resolved() {
			// Сняли с паузы, а ответа владельца так и нет — ждём дальше
			_, err := b.store.setTaskStatus(t.ID, "waiting", "running")
			return err
		}
		t.Transcript = append(t.Transcript, chatMsg{Role: "tool", ToolCallID: p.Calls[0].ID, Content: b.resolvePending(ctx, t, p)})
		rest := p.Calls[1:]
		t.Pending = nil
		if parked, err := b.taskCalls(ctx, t, rest); parked || err != nil {
			return err
		}
		if err := b.store.saveTaskProgress(t); err != nil {
			return err
		}
	}

	for {
		// Пауза и отмена — между шагами: шаг, начатый до нажатия, доделывается
		if st := b.store.taskStatus(t.ID); st != "running" {
			return nil
		}
		compactTranscript(t)
		msgs, tools := t.Transcript, b.taskSpecs
		last := t.Steps >= b.cfg.taskMaxSteps
		if last {
			tools = nil
			msgs = append(append([]chatMsg(nil), msgs...), chatMsg{Role: "user",
				Content: "(Шаги закончились. Подведи итог тем, что удалось сделать, и честно скажи, чего не успела.)"})
		}
		b.refreshCard(ctx, t)
		out, err := b.think(ctx, msgs, tools)
		if err != nil {
			return err
		}
		if len(out.ToolCalls) == 0 || last {
			if strings.TrimSpace(out.Content) == "" {
				return errBrainEmpty
			}
			return b.completeTask(ctx, t, out.Content)
		}
		t.Transcript = append(t.Transcript, out)
		parked, err := b.taskCalls(ctx, t, out.ToolCalls)
		if parked || err != nil {
			return err
		}
		if err := b.store.saveTaskProgress(t); err != nil {
			return err
		}
	}
}

// taskCalls выполняет вызовы одного хода модели. Вызов, которому нужен
// владелец, паркует задачу вместе с ещё не выполненными: parked = true.
func (b *bot) taskCalls(ctx context.Context, t *task, calls []toolCall) (parked bool, err error) {
	reply := func(tc toolCall, content string) {
		t.Transcript = append(t.Transcript, chatMsg{Role: "tool", ToolCallID: tc.ID, Content: content})
	}
	for i, tc := range calls {
		tl, args, bad := b.lookupTool(tc)
		if tl == nil {
			reply(tc, bad)
			continue
		}
		name := tc.Function.Name
		if name == "ask_user" {
			return true, b.parkInput(ctx, t, calls[i:], args)
		}
		if tl.confirm != nil {
			summary, err := tl.confirm(t.ChatID, args)
			if err != nil {
				reply(tc, "ошибка: "+err.Error())
				continue
			}
			return true, b.parkApproval(ctx, t, calls[i:], name, args, summary)
		}
		if tl.status != nil {
			t.Note = truncRunes(tl.status(args), 120)
			b.refreshCard(ctx, t)
		}
		t.Steps++
		reply(tc, b.execTool(ctx, t.ChatID, tl, name, args, 0))
	}
	return false, nil
}

func (b *bot) parkInput(ctx context.Context, t *task, calls []toolCall, args json.RawMessage) error {
	q := strings.TrimSpace(argStr(args, "question"))
	if q == "" {
		q = "Нужен твой ответ, чтобы продолжить."
	}
	p := &pendingCall{Calls: calls, Kind: "input", Question: q}
	var rows [][]tgButton
	for i, o := range argStrings(args, "options") {
		if i == 4 {
			break
		}
		o = truncRunes(strings.TrimSpace(o), 60)
		p.Options = append(p.Options, o)
		rows = append(rows, []tgButton{{Text: o, Data: fmt.Sprintf("ans:%d:%d", t.ID, i)}})
	}
	text := fmt.Sprintf("❓ Задача #%d спрашивает:\n%s\n\n", t.ID, q)
	if len(rows) > 0 {
		text += "Выбери вариант или ответь реплаем на это сообщение."
	} else {
		text += "Ответь реплаем на это сообщение."
	}
	msgID, err := b.tg.sendButtons(ctx, t.ChatID, text, rows)
	if err != nil {
		return err
	}
	t.Pending, t.AskMsgID, t.Note = p, msgID, ""
	return b.park(t)
}

func (b *bot) parkApproval(ctx context.Context, t *task, calls []toolCall, name string, args json.RawMessage, summary string) error {
	id, err := b.store.createAction(t.ChatID, t.ID, name, string(args), summary)
	if err != nil {
		return err
	}
	if _, err := b.tg.sendButtons(ctx, t.ChatID, approvalPrefix+summary+fmt.Sprintf("\n(задача #%d)", t.ID), approvalButtons(id)); err != nil {
		return err
	}
	t.Pending, t.Note = &pendingCall{Calls: calls, Kind: "approval", ActionID: id, Summary: summary}, ""
	return b.park(t)
}

func (b *bot) park(t *task) error {
	if err := b.store.saveTaskProgress(t); err != nil {
		return err
	}
	// Не вышло — значит, пока шли к владельцу, задачу поставили на паузу
	// или отменили; припаркованный вызов всё равно сохранён
	_, err := b.store.setTaskStatus(t.ID, "waiting", "running")
	return err
}

// resolvePending — результат припаркованного вызова для модели.
func (b *bot) resolvePending(ctx context.Context, t *task, p *pendingCall) string {
	if p.Kind == "input" {
		return "Владелец ответил: " + p.Answer
	}
	if p.Decision != "approved" {
		return rejectedResult
	}
	tl, args, bad := b.lookupTool(p.Calls[0])
	if tl == nil {
		return bad
	}
	t.Steps++
	return b.execTool(ctx, t.ChatID, tl, p.Calls[0].Function.Name, args, p.ActionID)
}

func (b *bot) completeTask(ctx context.Context, t *task, result string) error {
	t.Note = ""
	for i := range t.Plan {
		if t.Plan[i].Status == "doing" {
			t.Plan[i].Status = "done"
		}
	}
	if err := b.store.saveTaskProgress(t); err != nil {
		return err
	}
	ok, err := b.store.finishTask(t.ID, "done", result, "")
	if err != nil || !ok {
		return err // отменили в последний момент — итог не шлём
	}
	// В историю чата — чтобы можно было переспросить про итог
	if err := b.store.addMessage(t.ChatID, "user", fmt.Sprintf("Фоновая задача #%d: %s", t.ID, t.Goal), 0); err != nil {
		return err
	}
	if err := b.store.addMessage(t.ChatID, "assistant", result, t.Steps); err != nil {
		return err
	}
	if err := b.sendMarkdown(ctx, t.ChatID, fmt.Sprintf("📋 **Итог задачи #%d**\n\n%s", t.ID, result)); err != nil {
		return err
	}
	// Закончена крупная задача — тот самый повод для кружка. Карту ради
	// него не будим: она и так не спит, раз задача только что думала
	if b.store.getKV(circlesKey(t.ChatID)) != "off" {
		if err := b.circleFor(ctx, t.ChatID, result); err != nil && !errors.Is(err, errGPUAsleep) && !errors.Is(err, errNoVoice) {
			log.Printf("tasks: #%d: кружок: %v", t.ID, err)
		}
	}
	return nil
}

// compactTranscript сокращает старую выдачу инструментов, если ход задачи
// перестал влезать в контекст. Модель уже сделала из неё выводы — они в её
// же репликах; дословно держать старые страницы незачем.
func compactTranscript(t *task) {
	total := 0
	for _, m := range t.Transcript {
		total += utf8.RuneCountInString(m.Content)
	}
	for i := 0; i < len(t.Transcript)-taskKeepRecent && total > taskContextBudget; i++ {
		m := &t.Transcript[i]
		if m.Role != "tool" || utf8.RuneCountInString(m.Content) <= 500 {
			continue
		}
		before := utf8.RuneCountInString(m.Content)
		m.Content = truncRunes(m.Content, 400) + " [сокращено, чтобы влезть в контекст]"
		total -= before - utf8.RuneCountInString(m.Content)
	}
}

// ── Карточка ───────────────────────────────────────────────────────────────

var stepIcons = map[string]string{"done": "✅", "doing": "▶️", "skipped": "➖"}

func (b *bot) cardText(t *task) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 Задача #%d\n%s\n\n", t.ID, truncRunes(t.Goal, 300))
	switch t.Status {
	case "queued":
		sb.WriteString("⏳ В очереди")
		if !b.brainReady() {
			sb.WriteString(" — ждёт видеокарту")
		}
	case "running":
		sb.WriteString("⚙️ Работаю")
	case "waiting":
		if t.Pending != nil && t.Pending.Kind == "approval" {
			sb.WriteString("🤚 Жду подтверждения: " + truncRunes(t.Pending.Summary, 200))
		} else {
			sb.WriteString("❓ Жду твоего ответа")
		}
	case "paused":
		sb.WriteString("⏸ На паузе")
	case "done":
		sb.WriteString("✅ Готово")
	case "failed":
		sb.WriteString("⚠️ Не вышло: " + truncRunes(t.Error, 300))
	case "cancelled":
		sb.WriteString("✖️ Отменена")
	}
	if t.Steps > 0 {
		fmt.Fprintf(&sb, " · шагов: %d", t.Steps)
	}
	if len(t.Plan) > 0 {
		sb.WriteString("\n\nПлан:")
		for _, s := range t.Plan {
			icon := stepIcons[s.Status]
			if icon == "" {
				icon = "▫️"
			}
			fmt.Fprintf(&sb, "\n%s %s", icon, truncRunes(s.Title, 120))
		}
	}
	if t.Note != "" && (t.Status == "running" || t.Status == "queued") {
		sb.WriteString("\n\n" + t.Note)
	}
	return sb.String()
}

func (b *bot) cardButtons(t *task) [][]tgButton {
	btn := func(text, op string) tgButton {
		return tgButton{Text: text, Data: fmt.Sprintf("task:%d:%s", t.ID, op)}
	}
	switch t.Status {
	case "queued":
		row := []tgButton{btn("⏸ Пауза", "pause"), btn("✖️ Отменить", "cancel")}
		if !b.brainReady() {
			return [][]tgButton{{{Text: "🟢 Разбудить видеокарту", Data: "wake"}}, row}
		}
		return [][]tgButton{row}
	case "running", "waiting":
		return [][]tgButton{{btn("⏸ Пауза", "pause"), btn("✖️ Отменить", "cancel")}}
	case "paused":
		return [][]tgButton{{btn("▶️ Продолжить", "resume"), btn("✖️ Отменить", "cancel")}}
	case "failed", "cancelled":
		return [][]tgButton{{btn("🔁 Повторить", "retry")}}
	}
	return nil
}

// refreshCard перерисовывает карточку. Статус берём из базы: в памяти
// раннера он мог устареть — кнопку нажали посреди шага.
func (b *bot) refreshCard(ctx context.Context, t *task) {
	if st := b.store.taskStatus(t.ID); st != "" {
		t.Status = st
	}
	if t.Status == "failed" || t.Status == "done" {
		if fresh, err := b.store.getTask(t.ID); err == nil {
			t.Error, t.Result = fresh.Error, fresh.Result
		}
	}
	text, rows := b.cardText(t), b.cardButtons(t)
	if t.CardMsgID != 0 {
		err := b.tg.editText(ctx, t.ChatID, t.CardMsgID, text, rows)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			return
		}
		// карточку удалили руками — пришлём новую
	}
	id, err := b.tg.sendButtons(ctx, t.ChatID, text, rows)
	if err != nil {
		log.Printf("tasks: #%d: карточка: %v", t.ID, err)
		return
	}
	t.CardMsgID = id
	_ = b.store.saveTaskProgress(t)
}

// ── Создание, кнопки, ответы ───────────────────────────────────────────────

func (b *bot) startTask(ctx context.Context, chatID int64, goal string) (int64, error) {
	id, err := b.store.createTask(chatID, goal)
	if err != nil {
		return 0, err
	}
	t, err := b.store.getTask(id)
	if err != nil {
		return 0, err
	}
	b.refreshCard(ctx, t)
	b.tasks.poke()
	return id, nil
}

func (b *bot) taskCmd(ctx context.Context, chatID int64, goal string) error {
	if goal == "" {
		return b.tg.sendPlain(ctx, chatID, "Что сделать? /task сравни три Go-библиотеки для работы с sqlite: лицензии, активность, производительность")
	}
	_, err := b.startTask(ctx, chatID, goal)
	return err
}

func (b *bot) tasksCmd(ctx context.Context, chatID int64) error {
	ts, err := b.store.listTasks(chatID, 10)
	if err != nil {
		return err
	}
	if len(ts) == 0 {
		return b.tg.sendPlain(ctx, chatID, "Задач пока не было. Поставить: /task <что сделать> — или просто попроси о чём-то большом.")
	}
	var sb strings.Builder
	sb.WriteString("Задачи:\n")
	for _, t := range ts {
		fmt.Fprintf(&sb, "\n#%d %s — %s", t.ID, taskStatusWord[t.Status], truncRunes(t.Goal, 80))
	}
	sb.WriteString("\n\nКарточка каждой — выше в чате, управлять — её кнопками.")
	return b.tg.sendPlain(ctx, chatID, sb.String())
}

var taskStatusWord = map[string]string{
	"queued": "в очереди", "running": "в работе", "waiting": "ждёт тебя", "paused": "на паузе",
	"done": "готово", "failed": "не вышло", "cancelled": "отменена",
}

// onTaskButton — task:<id>:pause|resume|cancel|retry.
func (b *bot) onTaskButton(ctx context.Context, cq *tgCallback, id int64, op string) {
	t, err := b.store.getTask(id)
	if err != nil || t.ChatID != cq.Message.Chat.ID {
		b.tg.answerCallback(ctx, cq.ID, "Задачи нет")
		return
	}
	var ok bool
	switch op {
	case "pause":
		ok, err = b.store.setTaskStatus(id, "paused", "queued", "running", "waiting")
	case "resume":
		if ok, err = b.store.setTaskStatus(id, "queued", "paused"); ok {
			b.tasks.poke()
		}
	case "retry":
		if ok, err = b.store.setTaskStatus(id, "queued", "failed", "cancelled"); ok {
			b.tasks.poke()
		}
	case "cancel":
		if ok, err = b.store.setTaskStatus(id, "cancelled", "queued", "running", "waiting", "paused"); ok {
			b.tasks.cancel(id)
			// Висящее подтверждение больше ничего не решает; а «повторить»
			// продолжит задачу с отказом, а не с вечного ожидания
			if p := t.Pending; p != nil && p.Kind == "approval" && !p.resolved() {
				_, _ = b.store.decideAction(t.ChatID, p.ActionID, "expired")
				p.Decision = "expired"
				_ = b.store.saveTaskProgress(t)
			}
		}
	}
	if err != nil {
		log.Printf("tasks: #%d %s: %v", id, op, err)
	}
	if !ok {
		b.tg.answerCallback(ctx, cq.ID, "Уже неактуально")
	} else {
		b.tg.answerCallback(ctx, cq.ID, "")
	}
	if fresh, err := b.store.getTask(id); err == nil {
		b.refreshCard(ctx, fresh)
	}
}

// answerTaskInput — ответ владельца на вопрос задачи (кнопка или реплай).
func (b *bot) answerTaskInput(ctx context.Context, t *task, answer string) (bool, error) {
	p := t.Pending
	if p == nil || p.Kind != "input" || p.resolved() {
		return false, nil
	}
	p.Answer = answer
	ok, err := b.store.answerTask(t.ID, p)
	if err != nil || !ok {
		return false, err
	}
	_ = b.tg.editText(ctx, t.ChatID, t.AskMsgID, fmt.Sprintf("❓ Задача #%d спрашивала:\n%s\n\n→ %s", t.ID, p.Question, truncRunes(answer, 500)), nil)
	if fresh, err := b.store.getTask(t.ID); err == nil {
		b.refreshCard(ctx, fresh)
	}
	b.tasks.poke()
	return true, nil
}

// onAnswerButton — ans:<task>:<номер варианта>.
func (b *bot) onAnswerButton(ctx context.Context, cq *tgCallback, id int64, idx int) {
	t, err := b.store.getTask(id)
	if err != nil || t.ChatID != cq.Message.Chat.ID || t.Pending == nil || idx < 0 || idx >= len(t.Pending.Options) {
		b.tg.answerCallback(ctx, cq.ID, "Уже неактуально")
		return
	}
	ok, err := b.answerTaskInput(ctx, t, t.Pending.Options[idx])
	if err != nil {
		log.Printf("tasks: #%d ответ: %v", id, err)
	}
	if !ok {
		b.tg.answerCallback(ctx, cq.ID, "Уже неактуально")
		return
	}
	b.tg.answerCallback(ctx, cq.ID, "")
}

// resolveTaskApproval — решение по подтверждению задачи.
func (b *bot) resolveTaskApproval(ctx context.Context, taskID, actionID int64, decision string) {
	t, err := b.store.getTask(taskID)
	if err != nil || t.Pending == nil || t.Pending.ActionID != actionID || t.Pending.resolved() {
		return
	}
	t.Pending.Decision = decision
	if ok, err := b.store.answerTask(taskID, t.Pending); err != nil || !ok {
		return
	}
	if fresh, err := b.store.getTask(taskID); err == nil {
		b.refreshCard(ctx, fresh)
	}
	b.tasks.poke()
}

// ── Инструменты задач ──────────────────────────────────────────────────────

func (b *bot) taskTools() []*tool {
	return []*tool{
		{
			spec: toolSpecFunc{
				Name: "start_task",
				Description: "Поставить фоновую задачу — для долгой работы: исследование, сравнение, много поисков и страниц. " +
					"Владелец увидит карточку с планом, итог придёт отдельным сообщением. goal — полная самодостаточная формулировка: " +
					"задача не увидит этот разговор.",
				Parameters: object(map[string]any{"goal": str("что сделать, со всеми подробностями из разговора")}, "goal"),
			},
			modes:  inChat,
			status: func(json.RawMessage) string { return "📋 Ставлю фоновую задачу" },
			run: func(ctx context.Context, chatID int64, a json.RawMessage) (string, error) {
				if taskFrom(ctx) != nil {
					return "", errors.New("внутри задачи новые не ставлю — делай сама")
				}
				goal := strings.TrimSpace(argStr(a, "goal"))
				if goal == "" {
					return "", errors.New("пустой goal")
				}
				id, err := b.startTask(context.WithoutCancel(ctx), chatID, goal)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("Задача #%d поставлена. Владелец видит её карточку, итог придёт отдельным сообщением. Коротко скажи, что взялась.", id), nil
			},
		},
		{
			spec: toolSpecFunc{
				Name:        "update_plan",
				Description: "Записать или обновить план задачи — владелец видит его в карточке. Статусы: todo, doing, done, skipped.",
				Parameters: object(map[string]any{"steps": map[string]any{
					"type": "array", "minItems": 1, "maxItems": 10,
					"items": object(map[string]any{
						"title":  str("шаг, коротко"),
						"status": map[string]any{"type": "string", "enum": []string{"todo", "doing", "done", "skipped"}},
					}, "title", "status"),
				}}, "steps"),
			},
			modes: inTask,
			run: func(ctx context.Context, _ int64, a json.RawMessage) (string, error) {
				t := taskFrom(ctx)
				if t == nil {
					return "", errors.New("план бывает только у фоновой задачи")
				}
				var args struct {
					Steps []planStep `json:"steps"`
				}
				if err := json.Unmarshal(a, &args); err != nil {
					return "", err
				}
				if len(args.Steps) > 10 {
					args.Steps = args.Steps[:10]
				}
				t.Plan = args.Steps
				return "План обновлён.", nil
			},
		},
		{
			spec: toolSpecFunc{
				Name: "ask_user",
				Description: "Спросить владельца, когда без его ответа дальше никак (выбор, недостающие данные). Задача встанет на ожидание " +
					"и продолжится с его ответом. options — до 4 коротких вариантов для кнопок. Не спрашивай то, что можно выяснить самой.",
				Parameters: object(map[string]any{
					"question": str("вопрос, коротко и по делу"),
					"options":  map[string]any{"type": "array", "items": str(""), "maxItems": 4},
				}, "question"),
			},
			modes: inTask,
			// Сюда доходит только из чата: в задаче вызов паркуется раньше
			run: func(context.Context, int64, json.RawMessage) (string, error) {
				return "", errors.New("в чате просто спроси владельца в ответе")
			},
		},
	}
}

func argStrings(a json.RawMessage, key string) []string {
	var m map[string]any
	_ = json.Unmarshal(a, &m)
	arr, _ := m[key].([]any)
	var out []string
	for _, v := range arr {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(s), "#"), 10, 64)
	return id, err == nil
}
