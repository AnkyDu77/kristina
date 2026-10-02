package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
}

type bot struct {
	cfg   botConfig
	tg    *tgAPI
	llm   *llmClient
	media *mediaClient
	gpu   *gpuManager // nil — статический режим

	mu     sync.Mutex
	chats  map[int64]*chatState
	waking bool // уже ждём подъёма машины — второй раз не будим
}

// incoming — сообщение в очереди чата: текст и кто его прислал (от этого
// зависит, можно ли ему /wake и /sleep).
type incoming struct {
	from int64
	text string
}

type chatState struct {
	queue   chan incoming
	history []chatMsg
	noCircl bool // /circles off — только текст (бережём GPU)
}

func newBot(cfg botConfig, tg *tgAPI, llm *llmClient, media *mediaClient, gpu *gpuManager) *bot {
	return &bot{cfg: cfg, tg: tg, llm: llm, media: media, gpu: gpu, chats: map[int64]*chatState{}}
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
		cs = &chatState{queue: make(chan incoming, 32)}
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

// errGPUAsleep — видеокарта спит, а будить её сообщениями нельзя: только
// /wake от владельца.
var errGPUAsleep = errors.New("видеокарта спит")

func (b *bot) handle(ctx context.Context, chatID int64, cs *chatState, in incoming) {
	text := in.text
	cmd, arg, _ := strings.Cut(strings.TrimSpace(text), " ")
	cmd = strings.SplitN(cmd, "@", 2)[0] // /status@kristina_bot в группах
	arg = strings.TrimSpace(arg)

	var err error
	switch cmd {
	case "/start", "/help":
		err = b.tg.sendPlain(ctx, chatID, helpText)
	case "/reset":
		cs.history = nil
		err = b.tg.sendPlain(ctx, chatID, "Начинаем с чистого листа.")
	case "/status":
		err = b.tg.sendPlain(ctx, chatID, b.statusText(cs))
	case "/circles":
		cs.noCircl = arg == "off"
		state := "кружками"
		if cs.noCircl {
			state = "только текстом"
		}
		err = b.tg.sendPlain(ctx, chatID, "Отвечаю "+state+".")
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
		err = b.answer(ctx, chatID, cs, text)
	}
	if errors.Is(err, errGPUAsleep) {
		_ = b.tg.sendPlain(ctx, chatID, "Видеокарта спит — я пока не могу ни думать, ни говорить. Разбудить: /wake (только владелец).")
		return
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("bot: chat %d: %v", chatID, err)
		_ = b.tg.sendPlain(ctx, chatID, "Ошибка: "+err.Error())
	}
}

const helpText = `Привет! Я Кристина. Пиши — отвечу кружком, а код и подробности пришлю текстом.

/design <описание голоса> — подобрать мне голос (например: «молодая женщина, низкий спокойный тембр, говорит неторопливо»)
/keep — оставить последний подобранный голос
/circles on|off — отвечать кружками или только текстом
/wake, /sleep — разбудить или погасить видеокарту (только владелец; сама гаснет после простоя)
/status — что с видеокартой и голосом
/reset — забыть разговор`

// ── Ответ ──────────────────────────────────────────────────────────────────

func (b *bot) answer(ctx context.Context, chatID int64, cs *chatState, text string) error {
	// «Мозг» на GPU — без неё не ответить вовсе. Машину держим занятой до
	// конца ответа, чтобы watchdog не снёс её между LLM и кружком.
	// Внешний «мозг» отвечает и без GPU — она нужна только для кружка.
	gpuBase, release := "", func() {}
	llmBase := b.cfg.llmBase
	if llmBase == "" {
		var err error
		if gpuBase, release, err = b.requireGPU(ctx, chatID); err != nil {
			return err
		}
		defer release()
		llmBase = gpuBase + "/llm/v1"
	}

	stop := b.keepAction(ctx, chatID, "typing")
	msgs := make([]chatMsg, 0, len(cs.history)+2)
	msgs = append(msgs, chatMsg{Role: "system", Content: systemPrompt(b.cfg.maxSpeech)})
	msgs = append(msgs, cs.history...)
	msgs = append(msgs, chatMsg{Role: "user", Content: text})
	raw, err := b.llm.complete(ctx, llmBase, msgs)
	stop()
	if err != nil {
		return err
	}
	r := parseReply(raw, b.cfg.maxSpeech)

	// В историю кладём ответ в том же JSON-формате, которого ждём от
	// модели: увидев в истории прозу, она быстро забывает про формат
	norm, _ := json.Marshal(r)
	cs.history = append(cs.history, chatMsg{Role: "user", Content: text}, chatMsg{Role: "assistant", Content: string(norm)})
	if n := 2 * b.cfg.historyTurns; len(cs.history) > n {
		cs.history = cs.history[len(cs.history)-n:]
	}

	voice, verr := b.loadVoice()
	switch {
	case cs.noCircl:
		return b.sendText(ctx, chatID, r.Speech, r.Text)
	case verr != nil:
		_ = b.sendText(ctx, chatID, r.Speech, r.Text)
		return b.tg.sendPlain(ctx, chatID, "У меня пока нет голоса — подбери его через /design <описание>.")
	}

	base := gpuBase
	if base == "" {
		var ok bool
		var rel func()
		if base, rel, ok = b.acquireGPU(); !ok {
			// Машина спит, а «мозг» внешний: отвечаем хотя бы текстом.
			// Будить её сообщением нельзя — только /wake от владельца
			_ = b.sendText(ctx, chatID, r.Speech, r.Text)
			return b.tg.sendPlain(ctx, chatID, "Кружки — когда проснётся видеокарта (/wake).")
		}
		defer rel()
	}

	stop = b.keepAction(ctx, chatID, "record_video_note")
	mp4, err := b.circle(ctx, base, r.Speech, voice)
	stop()
	if err != nil {
		_ = b.sendText(ctx, chatID, r.Speech, r.Text)
		return fmt.Errorf("кружок не получился: %w", err)
	}
	if err := b.tg.sendVideoNote(ctx, chatID, mp4, b.cfg.circleSide); err != nil {
		return err
	}
	if r.Text != "" {
		return b.sendMarkdown(ctx, chatID, r.Text)
	}
	return nil
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

// sendText — запасной путь без кружка: речь и письменная часть одним
// сообщением.
func (b *bot) sendText(ctx context.Context, chatID int64, speech, text string) error {
	md := speech
	if text != "" {
		md += "\n\n" + text
	}
	return b.sendMarkdown(ctx, chatID, md)
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
// только /wake от владельца. Но если машина уже просыпается, ждём:
// сообщение, отправленное сразу после /wake, не должно получить «спит».
func (b *bot) requireGPU(ctx context.Context, chatID int64) (string, func(), error) {
	if base, release, ok := b.acquireGPU(); ok {
		return base, release, nil
	}
	if b.gpu.status().State != "provisioning" {
		return "", nil, errGPUAsleep
	}
	_ = b.tg.sendPlain(ctx, chatID, "Видеокарта просыпается — отвечу, как будет готова.")
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
		_ = b.tg.sendPlain(ctx, chatID, "Видеокарта готова — могу говорить 🎥")
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

func (b *bot) statusText(cs *chatState) string {
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
	if _, err := b.loadVoice(); err != nil {
		sb.WriteString("Голос: нет (/design)\n")
	} else {
		sb.WriteString("Голос: есть\n")
	}
	fmt.Fprintf(&sb, "Аватар: %s\n", filepath.Base(b.cfg.avatarPath))
	fmt.Fprintf(&sb, "Кружки: %v\n", !cs.noCircl)
	fmt.Fprintf(&sb, "История: %d сообщ.", len(cs.history))
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
