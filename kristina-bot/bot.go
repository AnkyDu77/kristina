package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// designSample — что Кристина говорит, когда пробуем новый голос. Эта же
// фраза становится расшифровкой эталона (ref_text) после /keep, поэтому
// она должна звучать естественно и покрывать побольше звуков.
const designSample = "Привет! Я Кристина. Давай разберёмся с твоим кодом: рассказывай, что сломалось, что уже пробовал и чего хочешь добиться."

type botConfig struct {
	allowed map[int64]bool
	// gpuAdmins — кто может будить и гасить видеокарту. Это прямые
	// деньги, поэтому отдельный список: остальные из allowed говорят с
	// Кристиной, только пока карту поднял кто-то из этих.
	gpuAdmins    map[int64]bool
	dataDir      string
	avatarPath   string
	language     string
	maxSpeech    int
	historyTurns int
	circleSide   int
	staticBase   string // медиа-API без terraform (машина поднята руками)
	// llmBase — внешний «мозг» (lmify, API). Пусто — LLM живёт на той же
	// GPU, что TTS и lip-sync: <медиа-API>/llm/v1 под тем же ключом.
	llmBase string
	// maxSteps — сколько раз модель может сходить в инструменты за ответ
	maxSteps int
	// circleMinTools — с какого числа вызовов инструментов задача
	// «крупная» и её итог стоит сказать кружком. Кружок — минута работы
	// карты, на каждый ответ его не тратим.
	circleMinTools  int
	approvalTimeout time.Duration
	tz              *time.Location
}

type bot struct {
	cfg    botConfig
	tg     *tgAPI
	llm    *llmClient
	media  *mediaClient
	gpu    *gpuManager // nil — статический режим
	store  *store
	search *searchClient
	fetch  *http.Client

	tools     map[string]*tool
	toolSpecs []toolSpec

	mu      sync.Mutex
	chats   map[int64]*chatState
	waiting map[int64]chan bool // id подтверждения → ждущий агент
	waking  bool                // уже ждём подъёма машины — второй раз не будим
	// hintedPrivacy — чаты, которым уже объяснили, почему кружок пришёл
	// обычным видео
	hintedPrivacy map[int64]bool
}

// incoming — сообщение в очереди чата: текст и кто его прислал (от этого
// зависит, можно ли ему /wake и /sleep). inbox — не сообщение, а сигнал
// «карта проснулась, ответь на отложенное».
type incoming struct {
	from  int64
	text  string
	inbox bool
}

type chatState struct {
	queue   chan incoming
	noCircl bool // /circles off — никаких кружков, даже за крупные задачи
}

func newBot(cfg botConfig, tg *tgAPI, llm *llmClient, media *mediaClient, gpu *gpuManager, st *store) *bot {
	if cfg.maxSteps <= 0 {
		cfg.maxSteps = 6
	}
	if cfg.approvalTimeout <= 0 {
		cfg.approvalTimeout = 10 * time.Minute
	}
	if cfg.tz == nil {
		cfg.tz = time.Local
	}
	b := &bot{cfg: cfg, tg: tg, llm: llm, media: media, gpu: gpu, store: st,
		search: newSearchClient(), fetch: newFetchClient(),
		chats: map[int64]*chatState{}, waiting: map[int64]chan bool{}, hintedPrivacy: map[int64]bool{}}
	b.buildTools()
	return b
}

func (b *bot) run(ctx context.Context) error {
	if err := b.tg.deleteWebhook(ctx); err != nil {
		return fmt.Errorf("deleteWebhook: %w", err)
	}
	me, err := b.tg.getMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	log.Printf("bot: @%s слушает", me.Username)

	if b.gpu != nil {
		b.gpu.setOnReady(func() { b.drainInbox(ctx) })
	}
	// Вопросы, отложенные прошлым процессом, — если думать уже есть чем
	if b.brainReady() {
		b.drainInbox(ctx)
	}

	var offset int64
	for ctx.Err() == nil {
		updates, err := b.tg.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("bot: getUpdates: %v", err)
				time.Sleep(3 * time.Second)
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if cq := u.CallbackQuery; cq != nil {
				if cq.From == nil || cq.Message == nil || !b.cfg.allowed[cq.From.ID] {
					continue
				}
				// Мимо очереди чата: в ней, возможно, стоит агент, который
				// ждёт именно этой кнопки
				go b.onCallback(ctx, cq)
				continue
			}
			m := u.Message
			if m == nil || m.From == nil || strings.TrimSpace(m.Text) == "" {
				continue
			}
			if !b.cfg.allowed[m.From.ID] {
				log.Printf("bot: чужой tg_id %d (@%s) — игнор", m.From.ID, m.From.Username)
				continue
			}
			b.enqueue(ctx, m.Chat.ID, incoming{from: m.From.ID, text: m.Text})
		}
	}
	return ctx.Err()
}

// enqueue — сообщения одного чата обрабатываются строго по очереди:
// история диалога общая, и два ответа наперегонки её перемешали бы.
func (b *bot) enqueue(ctx context.Context, chatID int64, in incoming) {
	b.mu.Lock()
	cs, ok := b.chats[chatID]
	if !ok {
		cs = &chatState{queue: make(chan incoming, 32), noCircl: b.store.getKV(circlesKey(chatID)) == "off"}
		b.chats[chatID] = cs
		go func() {
			for in := range cs.queue {
				b.handle(ctx, chatID, cs, in)
			}
		}()
	}
	b.mu.Unlock()
	select {
	case cs.queue <- in:
	default:
		_ = b.tg.sendPlain(ctx, chatID, "Не успеваю — подожди, пока отвечу на предыдущие.")
	}
}

func circlesKey(chatID int64) string { return "circles:" + strconv.FormatInt(chatID, 10) }

// errGPUAsleep — видеокарта спит, а будить её сообщениями нельзя: только
// владелец, командой или кнопкой.
var errGPUAsleep = errors.New("видеокарта спит")

func (b *bot) handle(ctx context.Context, chatID int64, cs *chatState, in incoming) {
	if in.inbox {
		if err := b.answer(ctx, chatID, cs, in); err != nil && ctx.Err() == nil {
			log.Printf("bot: chat %d: отложенное: %v", chatID, err)
			_ = b.tg.sendPlain(ctx, chatID, "Ошибка: "+err.Error())
		}
		return
	}
	text := in.text
	cmd, arg, _ := strings.Cut(strings.TrimSpace(text), " ")
	cmd = strings.SplitN(cmd, "@", 2)[0] // /status@kristina_bot в группах
	arg = strings.TrimSpace(arg)

	// Всё, кроме обычного сообщения, /circle и /design, работает без
	// видеокарты — ради памяти, статуса и настроек её не будим
	var err error
	switch cmd {
	case "/start", "/help":
		err = b.tg.sendPlain(ctx, chatID, helpText)
	case "/reset":
		if err = b.store.resetHistory(chatID); err == nil {
			err = b.tg.sendPlain(ctx, chatID, "Начинаем с чистого листа. Память о тебе (/memory) я не трогала.")
		}
	case "/status":
		err = b.tg.sendPlain(ctx, chatID, b.statusText(chatID, cs))
	case "/memory":
		err = b.showMemory(ctx, chatID)
	case "/remember":
		err = b.rememberCmd(ctx, chatID, arg)
	case "/forget":
		err = b.forgetCmd(ctx, chatID, arg)
	case "/circles":
		cs.noCircl = arg == "off"
		state := "on"
		if cs.noCircl {
			state = "off"
		}
		if err = b.store.setKV(circlesKey(chatID), state); err == nil {
			msg := "Кружок пришлю, только когда закончу крупную задачу (или попросишь: /circle)."
			if cs.noCircl {
				msg = "Только текст, кружков не будет. /circle всё равно работает."
			}
			err = b.tg.sendPlain(ctx, chatID, msg)
		}
	case "/circle":
		err = b.circleCmd(ctx, chatID)
	case "/wake", "/sleep":
		if !b.cfg.gpuAdmins[in.from] {
			log.Printf("bot: %s от tg_id %d — не владелец, отказ", cmd, in.from)
			err = b.tg.sendPlain(ctx, chatID, "Будить и гасить видеокарту может только владелец.")
			break
		}
		if cmd == "/wake" {
			b.wake(ctx, chatID)
		} else {
			err = b.sleep(ctx, chatID)
		}
	case "/design":
		err = b.design(ctx, chatID, arg)
	case "/keep":
		err = b.keepVoice(ctx, chatID)
	default:
		err = b.answer(ctx, chatID, cs, in)
	}
	if errors.Is(err, errGPUAsleep) {
		_ = b.tg.sendPlain(ctx, chatID, "Для этого нужна видеокарта, а она спит. Разбудить: /wake (только владелец).")
		return
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("bot: chat %d: %v", chatID, err)
		_ = b.tg.sendPlain(ctx, chatID, "Ошибка: "+err.Error())
	}
}

const helpText = `Привет! Я Кристина. Пиши — отвечу текстом; если нужно, сама поищу в интернете и прочту страницы. Закончу крупную задачу — скажу итог кружком.

Пока видеокарта спит, я записываю вопросы и отвечаю на все разом, когда её разбудят: так дешевле.

/memory — что я о тебе помню; /remember <факт> — запомнить; /forget <номер> — забыть
/circle — сказать последний ответ кружком
/circles on|off — кружки за крупные задачи: да или нет
/wake, /sleep — разбудить или погасить видеокарту (только владелец; сама гаснет после простоя)
/design <описание голоса> — подобрать мне голос; /keep — оставить его
/status — что с видеокартой, памятью и очередью
/reset — забыть разговор (память о тебе остаётся)`

// ── Ответ ──────────────────────────────────────────────────────────────────

func (b *bot) answer(ctx context.Context, chatID int64, cs *chatState, in incoming) error {
	text := in.text
	var inboxIDs []int64
	if in.inbox {
		items, err := b.store.pendingInbox(chatID)
		if err != nil || len(items) == 0 {
			return err
		}
		text = joinInbox(items)
		for _, it := range items {
			inboxIDs = append(inboxIDs, it.ID)
		}
	}
	if !b.brainReady() {
		if in.inbox {
			return nil // карта снова уснула — дождёмся следующего подъёма
		}
		return b.postpone(ctx, chatID, in)
	}

	hist, err := b.store.history(chatID, 2*b.cfg.historyTurns)
	if err != nil {
		return err
	}
	mems, err := b.store.memories(chatID)
	if err != nil {
		return err
	}
	msgs := make([]chatMsg, 0, len(hist)+2)
	msgs = append(msgs, chatMsg{Role: "system", Content: systemPrompt(mems, time.Now().In(b.cfg.tz))})
	msgs = append(msgs, hist...)
	msgs = append(msgs, chatMsg{Role: "user", Content: text})

	stop := b.keepAction(ctx, chatID, "typing")
	ans, steps, err := b.runAgent(ctx, chatID, msgs)
	stop()
	if errors.Is(err, errGPUAsleep) {
		// Карту погасили посреди ответа (watchdog, /sleep) — вопрос не теряем
		if !in.inbox {
			if _, err := b.store.addInbox(chatID, in.from, text); err != nil {
				return err
			}
		}
		return b.tg.sendPlain(ctx, chatID, "Видеокарта уснула посреди ответа. Вопрос записала — отвечу, как проснётся.")
	}
	if err != nil {
		return err
	}

	if err := b.store.addMessage(chatID, "user", text, 0); err != nil {
		return err
	}
	if err := b.store.addMessage(chatID, "assistant", ans, steps); err != nil {
		return err
	}
	if err := b.store.doneInbox(inboxIDs); err != nil {
		return err
	}
	if err := b.sendMarkdown(ctx, chatID, ans); err != nil {
		return err
	}

	// Кружок — только за крупную задачу и только если карта уже не спит:
	// ради кружка её не будим
	if !cs.noCircl && b.cfg.circleMinTools > 0 && steps >= b.cfg.circleMinTools {
		if err := b.circleFor(ctx, chatID, ans); err != nil && !errors.Is(err, errGPUAsleep) && !errors.Is(err, errNoVoice) {
			log.Printf("bot: chat %d: кружок за задачу: %v", chatID, err)
		}
	}
	return nil
}

// joinInbox — отложенные вопросы одним сообщением: один прогон модели
// вместо нескольких.
func joinInbox(items []inboxItem) string {
	if len(items) == 1 {
		return items[0].Text
	}
	var sb strings.Builder
	sb.WriteString("Пока видеокарта спала, я написал тебе несколько сообщений — ответь на все:\n")
	for i, it := range items {
		fmt.Fprintf(&sb, "\n%d. %s", i+1, it.Text)
	}
	return sb.String()
}

// postpone откладывает вопрос до подъёма карты. Будить её — решение
// владельца: кнопка под первым отложенным вопросом.
func (b *bot) postpone(ctx context.Context, chatID int64, in incoming) error {
	n, err := b.store.addInbox(chatID, in.from, in.text)
	if err != nil {
		return err
	}
	if b.gpu != nil && b.gpu.status().State == "provisioning" {
		return b.tg.sendPlain(ctx, chatID, "Видеокарта просыпается — отвечу, как будет готова.")
	}
	if n > 1 {
		return b.tg.sendPlain(ctx, chatID, fmt.Sprintf("Записала (в очереди: %d).", n))
	}
	msg := "Видеокарта спит, думать мне пока нечем. Вопрос записала — отвечу на всё разом, как она проснётся, так что можно накидать ещё."
	if !b.cfg.gpuAdmins[in.from] {
		return b.tg.sendPlain(ctx, chatID, msg+" Разбудить её может владелец.")
	}
	_, err = b.tg.sendButtons(ctx, chatID, msg, [][]tgButton{{{Text: "🟢 Разбудить сейчас", Data: "wake"}}})
	return err
}

// drainInbox — карта проснулась: в каждый чат с отложенными вопросами
// ставим в очередь «ответь на отложенное».
func (b *bot) drainInbox(ctx context.Context) {
	chats, err := b.store.inboxChats()
	if err != nil {
		log.Printf("bot: inbox: %v", err)
		return
	}
	for _, chatID := range chats {
		b.enqueue(ctx, chatID, incoming{inbox: true})
	}
}

// ── Кнопки ─────────────────────────────────────────────────────────────────

func (b *bot) onCallback(ctx context.Context, cq *tgCallback) {
	chatID, msgID := cq.Message.Chat.ID, cq.Message.MessageID
	switch {
	case cq.Data == "wake":
		if !b.cfg.gpuAdmins[cq.From.ID] {
			b.tg.answerCallback(ctx, cq.ID, "Будить видеокарту может только владелец")
			return
		}
		b.tg.answerCallback(ctx, cq.ID, "")
		_ = b.tg.editText(ctx, chatID, msgID, cq.Message.Text, nil) // кнопка своё отработала
		b.wake(ctx, chatID)

	case strings.HasPrefix(cq.Data, "act:"):
		// act:<id>:ok|no
		parts := strings.Split(cq.Data, ":")
		if len(parts) != 3 {
			b.tg.answerCallback(ctx, cq.ID, "")
			return
		}
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			b.tg.answerCallback(ctx, cq.ID, "")
			return
		}
		approved := parts[2] == "ok"
		status, mark := "rejected", "❌ Не стала: "
		if approved {
			status, mark = "approved", "✅ Да: "
		}
		won, err := b.store.decideAction(chatID, id, status)
		if err != nil {
			log.Printf("bot: подтверждение %d: %v", id, err)
		}
		summary := strings.TrimPrefix(cq.Message.Text, approvalPrefix)
		if !won {
			b.tg.answerCallback(ctx, cq.ID, "Уже неактуально")
			_ = b.tg.editText(ctx, chatID, msgID, "⌛ "+summary, nil)
			return
		}
		b.tg.answerCallback(ctx, cq.ID, "")
		_ = b.tg.editText(ctx, chatID, msgID, mark+summary, nil)
		b.mu.Lock()
		ch := b.waiting[id]
		b.mu.Unlock()
		if ch != nil {
			ch <- approved // буфер на одно решение, а решение ровно одно
		}

	default:
		b.tg.answerCallback(ctx, cq.ID, "")
	}
}

// ── Память ─────────────────────────────────────────────────────────────────

func (b *bot) showMemory(ctx context.Context, chatID int64) error {
	mems, err := b.store.memories(chatID)
	if err != nil {
		return err
	}
	if len(mems) == 0 {
		return b.tg.sendPlain(ctx, chatID, "Я пока ничего о тебе не помню. Расскажи — или /remember <факт>.")
	}
	var sb strings.Builder
	sb.WriteString("Что я о тебе помню:\n")
	for _, m := range mems {
		fmt.Fprintf(&sb, "\n#%d %s", m.ID, m.Text)
	}
	sb.WriteString("\n\nЗабыть: /forget <номер>")
	return b.tg.sendPlain(ctx, chatID, sb.String())
}

// rememberCmd — владелец сам диктует факт: подтверждать нечего.
func (b *bot) rememberCmd(ctx context.Context, chatID int64, text string) error {
	if text == "" {
		return b.tg.sendPlain(ctx, chatID, "Что запомнить? /remember пишу на Go, терпеть не могу YAML")
	}
	id, err := b.store.addMemory(chatID, text, "user")
	if err != nil {
		return err
	}
	return b.tg.sendPlain(ctx, chatID, fmt.Sprintf("Запомнила (#%d).", id))
}

func (b *bot) forgetCmd(ctx context.Context, chatID int64, arg string) error {
	id, err := strconv.ParseInt(strings.TrimPrefix(arg, "#"), 10, 64)
	if err != nil {
		return b.tg.sendPlain(ctx, chatID, "Какую запись забыть? /forget <номер из /memory>")
	}
	ok, err := b.store.forgetMemory(chatID, id)
	if err != nil {
		return err
	}
	if !ok {
		return b.tg.sendPlain(ctx, chatID, fmt.Sprintf("Записи #%d нет.", id))
	}
	return b.tg.sendPlain(ctx, chatID, fmt.Sprintf("Забыла #%d.", id))
}

// ── Кружки ─────────────────────────────────────────────────────────────────

var errNoVoice = errors.New("у меня пока нет голоса — подбери его через /design <описание>")

func (b *bot) circleCmd(ctx context.Context, chatID int64) error {
	ans, err := b.store.lastAnswer(chatID)
	if err != nil {
		return err
	}
	if ans == "" {
		return b.tg.sendPlain(ctx, chatID, "Мне пока нечего сказать — сначала спроси что-нибудь.")
	}
	err = b.circleFor(ctx, chatID, ans)
	if errors.Is(err, errGPUAsleep) {
		return b.tg.sendPlain(ctx, chatID, "Кружки — когда проснётся видеокарта (/wake).")
	}
	return err
}

// circleFor говорит суть ответа кружком. Карту не будит: спит — errGPUAsleep.
func (b *bot) circleFor(ctx context.Context, chatID int64, answer string) error {
	voice, err := b.loadVoice()
	if err != nil {
		return errNoVoice
	}
	base, release, ok := b.acquireGPU()
	if !ok {
		return errGPUAsleep
	}
	defer release()

	stop := b.keepAction(ctx, chatID, "record_video_note")
	defer stop()
	llmBase := b.cfg.llmBase
	if llmBase == "" {
		llmBase = base + "/llm/v1"
	}
	out, err := b.llm.chat(ctx, llmBase, []chatMsg{
		{Role: "system", Content: speechPrompt(b.cfg.maxSpeech)},
		{Role: "user", Content: answer},
	}, nil)
	if err != nil {
		return err
	}
	speech := clampSpeech(speechFrom(out.Content), b.cfg.maxSpeech)
	if speech == "" {
		return errBrainEmpty
	}
	mp4, err := b.circle(ctx, base, speech, voice)
	if err != nil {
		return fmt.Errorf("кружок не получился: %w", err)
	}
	return b.sendCircle(ctx, chatID, mp4)
}

func (b *bot) circle(ctx context.Context, base, speech string, voice voiceRef) ([]byte, error) {
	wav, err := b.media.speak(ctx, base, speech, b.cfg.language, voice)
	if err != nil {
		return nil, fmt.Errorf("TTS: %w", err)
	}
	avatarID, err := b.media.ensureAvatar(ctx, base, b.cfg.avatarPath)
	if err != nil {
		return nil, fmt.Errorf("аватар: %w", err)
	}
	mp4, err := b.media.render(ctx, base, avatarID, wav)
	if err != nil {
		return nil, fmt.Errorf("lip-sync: %w", err)
	}
	return mp4, nil
}

// sendCircle шлёт кружок, а если Telegram его не принял из-за настроек
// приватности получателя, — то же видео обычным сообщением.
func (b *bot) sendCircle(ctx context.Context, chatID int64, mp4 []byte) error {
	err := b.tg.sendVideoNote(ctx, chatID, mp4, b.cfg.circleSide)
	if err == nil || !strings.Contains(err.Error(), "VOICE_MESSAGES_FORBIDDEN") {
		return err
	}
	caption := ""
	b.mu.Lock()
	if !b.hintedPrivacy[chatID] {
		// Подсказку — один раз на чат: дальше она только шумела бы
		b.hintedPrivacy[chatID] = true
		caption = "Кружки запрещены твоими настройками приватности, поэтому шлю обычным видео. " +
			"Включить: Настройки → Конфиденциальность → Голосовые сообщения → Исключения → Всегда разрешать → этот бот."
	}
	b.mu.Unlock()
	return b.tg.sendVideo(ctx, chatID, mp4, b.cfg.circleSide, caption)
}

func (b *bot) sendMarkdown(ctx context.Context, chatID int64, md string) error {
	for _, chunk := range splitMarkdown(md, tgChunkRunes) {
		if err := b.tg.sendHTML(ctx, chatID, markdownToHTML(chunk)); err != nil {
			// Разметка не понравилась телеграму — лучше без неё, чем никак
			log.Printf("bot: HTML не прошёл (%v) — шлю без разметки", err)
			if err := b.tg.sendPlain(ctx, chatID, chunk); err != nil {
				return err
			}
		}
	}
	return nil
}

// keepAction держит статус «печатает…»/«записывает видео…», пока идёт
// долгая операция: сам по себе он гаснет через 5 секунд.
func (b *bot) keepAction(ctx context.Context, chatID int64, action string) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		for {
			b.tg.chatAction(ctx, chatID, action)
			select {
			case <-ctx.Done():
				return
			case <-time.After(4 * time.Second):
			}
		}
	}()
	return cancel
}

// ── GPU ────────────────────────────────────────────────────────────────────

func (b *bot) acquireGPU() (base string, release func(), ok bool) {
	if b.gpu == nil {
		return b.cfg.staticBase, func() {}, true
	}
	return b.gpu.acquire()
}

// requireGPU занимает машину под запрос. Сама её не будит — это делает
// только владелец. Но если машина уже просыпается, ждём: /design,
// отправленный сразу после /wake, не должен получить «спит».
func (b *bot) requireGPU(ctx context.Context, chatID int64) (string, func(), error) {
	if base, release, ok := b.acquireGPU(); ok {
		return base, release, nil
	}
	if b.gpu.status().State != "provisioning" {
		return "", nil, errGPUAsleep
	}
	_ = b.tg.sendPlain(ctx, chatID, "Видеокарта просыпается — сделаю, как будет готова.")
	wctx, cancel := context.WithTimeout(ctx, 90*time.Minute)
	defer cancel()
	if err := b.gpu.waitRunning(wctx); err != nil {
		return "", nil, err
	}
	if base, release, ok := b.acquireGPU(); ok {
		return base, release, nil
	}
	return "", nil, errGPUAsleep
}

// wake поднимает машину и сообщает в чат, когда она готова. Ждём в
// фоне: очередь чата не должна стоять пять минут ради одного уведомления.
func (b *bot) wake(ctx context.Context, chatID int64) {
	if b.gpu == nil {
		_ = b.tg.sendPlain(ctx, chatID, "Медиа-API задан статически (KRISTINA_MEDIA_BASE) — будить нечего.")
		return
	}
	b.mu.Lock()
	if b.waking {
		b.mu.Unlock()
		return
	}
	b.waking = true
	b.mu.Unlock()

	if err := b.gpu.start(); err != nil && b.gpu.status().State != "provisioning" {
		b.mu.Lock()
		b.waking = false
		b.mu.Unlock()
		_ = b.tg.sendPlain(ctx, chatID, "Видеокарта: "+err.Error())
		return
	}
	_ = b.tg.sendPlain(ctx, chatID, "Бужу видеокарту — обычно это 5–10 минут. Напишу, как проснётся; сообщения можно слать уже сейчас — дождутся.")

	go func() {
		defer func() {
			b.mu.Lock()
			b.waking = false
			b.mu.Unlock()
		}()
		wctx, cancel := context.WithTimeout(ctx, 90*time.Minute)
		defer cancel()
		if err := b.gpu.waitRunning(wctx); err != nil {
			msg := "Видеокарта не поднялась: " + err.Error()
			if tail := lastLines(b.gpu.logTail(), 15); tail != "" {
				msg += "\n\n" + tail
			}
			_ = b.tg.sendPlain(ctx, chatID, msg)
			return
		}
		msg := "Видеокарта готова."
		if n := b.store.countInbox(chatID); n > 0 {
			msg += fmt.Sprintf(" Отвечаю на отложенное (%d).", n)
		}
		_ = b.tg.sendPlain(ctx, chatID, msg)
	}()
}

func (b *bot) sleep(ctx context.Context, chatID int64) error {
	if b.gpu == nil {
		return errors.New("медиа-API задан статически — гасить нечего")
	}
	if err := b.gpu.stop(); err != nil {
		return err
	}
	return b.tg.sendPlain(ctx, chatID, "Гашу видеокарту.")
}

func (b *bot) statusText(chatID int64, cs *chatState) string {
	var sb strings.Builder
	if b.gpu == nil {
		fmt.Fprintf(&sb, "Медиа-API: %s (статически)\n", b.cfg.staticBase)
	} else {
		st := b.gpu.status()
		fmt.Fprintf(&sb, "Видеокарта: %s\n", st.State)
		if st.State == "running" {
			fmt.Fprintf(&sb, "  работает %s, авто-стоп через %s\n", st.Up.Round(time.Minute), st.IdleLeft.Round(time.Minute))
		}
		if st.Err != "" {
			fmt.Fprintf(&sb, "  ошибка: %s\n", st.Err)
		}
	}
	if b.cfg.llmBase == "" {
		sb.WriteString("Мозг: на той же видеокарте\n")
	} else {
		fmt.Fprintf(&sb, "Мозг: %s\n", b.cfg.llmBase)
	}
	if n := b.store.countInbox(chatID); n > 0 {
		fmt.Fprintf(&sb, "Отложено вопросов: %d\n", n)
	}
	if mems, err := b.store.memories(chatID); err == nil {
		fmt.Fprintf(&sb, "Памяти: %d записей\n", len(mems))
	}
	fmt.Fprintf(&sb, "История: %d сообщ.\n", b.store.countMessages(chatID))
	if _, err := b.loadVoice(); err != nil {
		sb.WriteString("Голос: нет (/design)\n")
	} else {
		sb.WriteString("Голос: есть\n")
	}
	fmt.Fprintf(&sb, "Аватар: %s\n", filepath.Base(b.cfg.avatarPath))
	if cs.noCircl {
		sb.WriteString("Кружки: выключены")
	} else {
		fmt.Fprintf(&sb, "Кружки: за задачи от %d шагов", b.cfg.circleMinTools)
	}
	return sb.String()
}

// ── Голос ──────────────────────────────────────────────────────────────────

func (b *bot) voicePath(name string) string { return filepath.Join(b.cfg.dataDir, "voice", name) }

func (b *bot) loadVoice() (voiceRef, error) {
	wav, err := os.ReadFile(b.voicePath("ref.wav"))
	if err != nil {
		return voiceRef{}, err
	}
	text, err := os.ReadFile(b.voicePath("ref.txt"))
	if err != nil {
		return voiceRef{}, err
	}
	return voiceRef{WAV: wav, Text: strings.TrimSpace(string(text))}, nil
}

func (b *bot) design(ctx context.Context, chatID int64, instruct string) error {
	if instruct == "" {
		return b.tg.sendPlain(ctx, chatID, "Опиши голос: /design молодая женщина, низкий спокойный тембр, говорит неторопливо")
	}
	base, release, err := b.requireGPU(ctx, chatID)
	if err != nil {
		return err
	}
	defer release()

	stop := b.keepAction(ctx, chatID, "record_voice")
	wav, err := b.media.design(ctx, base, designSample, instruct, b.cfg.language)
	stop()
	if err != nil {
		return fmt.Errorf("подбор голоса: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(b.voicePath("x")), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(b.voicePath("candidate.wav"), wav, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(b.voicePath("candidate.txt"), []byte(designSample), 0o644); err != nil {
		return err
	}
	return b.tg.sendAudio(ctx, chatID, "kristina-voice.wav", wav, "Нравится? /keep — оставить этот голос, или /design с другим описанием.")
}

func (b *bot) keepVoice(ctx context.Context, chatID int64) error {
	for _, ext := range []string{".wav", ".txt"} {
		if err := os.Rename(b.voicePath("candidate"+ext), b.voicePath("ref"+ext)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return b.tg.sendPlain(ctx, chatID, "Нечего оставлять — сначала /design <описание>.")
			}
			return err
		}
	}
	return b.tg.sendPlain(ctx, chatID, "Запомнила голос. Теперь говорю им.")
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
