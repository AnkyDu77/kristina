package main

// Мониторы страниц: «напиши, когда изменится / появится / пропадёт».
// Работают целиком на VPS бота, без LLM и без видеокарты: скачать,
// сравнить, написать. Карту не будит даже срабатывание — под алертом
// кнопка «🧠 Разобрать»; нажал — вопрос уходит Кристине обычным путём,
// а если карта спит, то в очередь.
//
// Алерт — только на смену состояния (было «нет» → стало «есть»), поэтому
// одно и то же событие не приходит каждый час. Сбои — с отступом: страница
// лежит — проверяем всё реже, на третьем сбое подряд пишем один раз.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	monitorTick        = 30 * time.Second
	monitorMaxBackoff  = 24 * time.Hour
	monitorAlertFails  = 3
	monitorMaxLastText = 100000
	monitorDiffLines   = 8
)

var monitorKinds = map[string]string{
	"change":     "изменится",
	"appears":    "появится",
	"disappears": "пропадёт",
}

func (b *bot) monitorLoop(ctx context.Context) {
	tick := time.NewTicker(monitorTick)
	defer tick.Stop()
	for {
		due, err := b.store.dueMonitors(time.Now())
		if err != nil {
			log.Printf("monitors: %v", err)
		}
		for _, m := range due {
			if ctx.Err() != nil {
				return
			}
			if alert := b.checkMonitor(ctx, m); alert != "" {
				b.sendMonitorAlert(ctx, m, alert)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// checkMonitor проверяет страницу, сохраняет новое состояние и
// возвращает текст алерта (пусто — писать не о чем).
func (b *bot) checkMonitor(ctx context.Context, m *monitor) (alert string) {
	text, err := b.monitorText(ctx, m.URL)
	now := time.Now()
	defer func() {
		if err := b.store.saveMonitor(m); err != nil {
			log.Printf("monitors: #%d: %v", m.ID, err)
		}
	}()

	if err != nil {
		m.Fails++
		m.LastError = err.Error()
		m.NextRun = now.Add(backoff(m.Interval, m.Fails))
		if m.Fails == monitorAlertFails {
			return fmt.Sprintf("⚠️ Не могу открыть страницу уже %d раза подряд: %s\nПроверяю реже; напишу, когда снова откроется.", m.Fails, truncRunes(err.Error(), 300))
		}
		return ""
	}
	if m.Fails >= monitorAlertFails {
		alert = "✅ Страница снова открывается.\n"
	}
	m.Fails, m.LastError = 0, ""
	m.NextRun = now.Add(m.Interval)

	old := m.State
	switch m.Kind {
	case "change":
		sum := sha256.Sum256([]byte(text))
		m.State = hex.EncodeToString(sum[:8])
		prev := m.LastText
		m.LastText = truncRunes(text, monitorMaxLastText)
		if old != "" && old != m.State {
			alert += "🔔 Страница изменилась.\n" + textDiff(prev, text)
		}
	case "appears", "disappears":
		present := containsFold(text, m.Pattern)
		m.State = "0"
		if present {
			m.State = "1"
		}
		switch {
		case m.Kind == "appears" && old == "0" && present:
			alert += fmt.Sprintf("🔔 На странице появилось «%s».", m.Pattern)
		case m.Kind == "disappears" && old == "1" && !present:
			alert += fmt.Sprintf("🔔 Со страницы пропало «%s».", m.Pattern)
		}
	}
	if strings.HasPrefix(strings.TrimSpace(alert), "✅") && !strings.Contains(alert, "🔔") {
		alert = strings.TrimSpace(alert)
	}
	if alert != "" {
		m.LastAlert = alert
	}
	return alert
}

// monitorText — текст страницы. Без Parallel: монитор ходит часто, а
// бесплатный лимит чужого сервиса не для этого. Страница, которую рисует
// JS, сейчас не мониторится (браузер — в фазе 3).
func (b *bot) monitorText(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout+5*time.Second)
	defer cancel()
	p, err := fetchPage(ctx, b.fetch, url)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Text) == "" {
		return "", errors.New("на странице нет текста — видимо, её рисует JavaScript")
	}
	return p.Text, nil
}

func backoff(interval time.Duration, fails int) time.Duration {
	d := interval
	for i := 1; i < fails && d < monitorMaxBackoff; i++ {
		d *= 2
	}
	return min(d, monitorMaxBackoff)
}

func containsFold(text, pattern string) bool {
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	return pattern != "" && strings.Contains(norm(text), norm(pattern))
}

// textDiff — какие строки появились (а если только пропадали — какие
// пропали): алерт должен сразу показать суть, а не «что-то поменялось».
func textDiff(prev, cur string) string {
	set := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, l := range strings.Split(s, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				m[l] = true
			}
		}
		return m
	}
	pick := func(s string, other map[string]bool) []string {
		var out []string
		seen := map[string]bool{}
		for _, l := range strings.Split(s, "\n") {
			l = strings.TrimSpace(l)
			if l == "" || other[l] || seen[l] {
				continue
			}
			seen[l] = true
			out = append(out, "• "+truncRunes(l, 200))
		}
		return out
	}
	format := func(head string, lines []string) string {
		extra := ""
		if len(lines) > monitorDiffLines {
			extra = fmt.Sprintf("\n…и ещё %d", len(lines)-monitorDiffLines)
			lines = lines[:monitorDiffLines]
		}
		return head + "\n" + strings.Join(lines, "\n") + extra
	}
	if added := pick(cur, set(prev)); len(added) > 0 {
		return format("Появилось:", added)
	}
	if removed := pick(prev, set(cur)); len(removed) > 0 {
		return format("Пропало:", removed)
	}
	return "Поменялся порядок или мелочи в тексте."
}

func (b *bot) sendMonitorAlert(ctx context.Context, m *monitor, alert string) {
	text := fmt.Sprintf("Монитор #%d · %s\n\n%s", m.ID, m.URL, alert)
	btn := func(t, op string) tgButton { return tgButton{Text: t, Data: fmt.Sprintf("mon:%d:%s", m.ID, op)} }
	rows := [][]tgButton{{btn("🧠 Разобрать", "ask")}, {btn("⏸ Пауза", "pause"), btn("✖️ Удалить", "del")}}
	if _, err := b.tg.sendButtons(ctx, m.ChatID, truncRunes(text, 3500), rows); err != nil {
		log.Printf("monitors: #%d: алерт: %v", m.ID, err)
	}
}

// ── Создание ───────────────────────────────────────────────────────────────

// addMonitor заводит монитор и сразу проверяет страницу: битый адрес или
// JS-страница выяснятся сейчас, а не через час молчания. Заодно это
// точка отсчёта — алерт будет только на изменение после неё.
func (b *bot) addMonitor(ctx context.Context, m *monitor) (string, error) {
	if _, err := checkURL(m.URL); err != nil {
		return "", err
	}
	if monitorKinds[m.Kind] == "" {
		return "", fmt.Errorf("не знаю вида монитора %q", m.Kind)
	}
	if m.Kind != "change" && strings.TrimSpace(m.Pattern) == "" {
		return "", errors.New("для «появится/пропадёт» нужен текст, который ищем")
	}
	if m.Interval < b.cfg.monitorMin {
		m.Interval = b.cfg.monitorMin
	}
	text, err := b.monitorText(ctx, m.URL)
	if err != nil {
		return "", fmt.Errorf("не смогла открыть страницу: %w", err)
	}
	m.Status, m.NextRun = "active", time.Now().Add(m.Interval)
	id, err := b.store.createMonitor(m)
	if err != nil {
		return "", err
	}
	m.ID = id
	// Начальное состояние — тем же путём, что и обычная проверка
	m.State, m.LastText = "", ""
	if m.Kind != "change" {
		m.State = "0"
		if containsFold(text, m.Pattern) {
			m.State = "1"
		}
	} else {
		sum := sha256.Sum256([]byte(text))
		m.State, m.LastText = hex.EncodeToString(sum[:8]), truncRunes(text, monitorMaxLastText)
	}
	if err := b.store.saveMonitor(m); err != nil {
		return "", err
	}

	desc := fmt.Sprintf("Монитор #%d: проверяю %s каждые %s, напишу, когда ", id, m.URL, humanInterval(m.Interval))
	switch m.Kind {
	case "change":
		desc += "страница изменится."
	case "appears":
		desc += fmt.Sprintf("появится «%s».", m.Pattern)
		if m.State == "1" {
			desc += " Сейчас этот текст на странице уже есть — напишу, если он пропадёт и появится снова."
		}
	case "disappears":
		desc += fmt.Sprintf("пропадёт «%s».", m.Pattern)
		if m.State == "0" {
			desc += " Сейчас этого текста на странице нет — напишу, если он появится и снова пропадёт."
		}
	}
	return desc, nil
}

func humanInterval(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%d ч", int(d/time.Hour))
	}
	return fmt.Sprintf("%d мин", int(d/time.Minute))
}

var (
	intervalRe = regexp.MustCompile(`^(\d+)\s*(m|min|мин|h|ч)$`)
	patternRe  = regexp.MustCompile(`(!?)\s*["«“]([^"»”]+)["»”]`)
)

// parseWatch разбирает /watch <url> [30m|2h] ["текст" | !"текст"].
func parseWatch(arg string, def time.Duration) (*monitor, error) {
	m := &monitor{Kind: "change", Interval: def}
	if pm := patternRe.FindStringSubmatch(arg); pm != nil {
		m.Kind, m.Pattern = "appears", strings.TrimSpace(pm[2])
		if pm[1] == "!" {
			m.Kind = "disappears"
		}
		arg = strings.Replace(arg, pm[0], " ", 1)
	}
	for _, f := range strings.Fields(arg) {
		if im := intervalRe.FindStringSubmatch(strings.ToLower(f)); im != nil {
			n, _ := strconv.Atoi(im[1])
			unit := time.Minute
			if im[2] == "h" || im[2] == "ч" {
				unit = time.Hour
			}
			m.Interval = time.Duration(n) * unit
			continue
		}
		if m.URL != "" {
			return nil, fmt.Errorf("не поняла %q", f)
		}
		m.URL = f
	}
	if m.URL == "" {
		return nil, errors.New("нет адреса")
	}
	return m, nil
}

const watchHelp = `Следить за страницей (без видеокарты — просто проверяю и пишу):
/watch https://… — когда изменится
/watch https://… "В наличии" — когда текст появится
/watch https://… !"Нет в наличии" — когда текст пропадёт
Интервал — например 30m или 2h (по умолчанию 1h).
/watches — список, /unwatch <номер> — удалить.`

func (b *bot) watchCmd(ctx context.Context, chatID int64, arg string) error {
	if arg == "" {
		return b.tg.sendPlain(ctx, chatID, watchHelp)
	}
	m, err := parseWatch(arg, b.cfg.monitorDefault)
	if err != nil {
		return b.tg.sendPlain(ctx, chatID, err.Error()+"\n\n"+watchHelp)
	}
	m.ChatID = chatID
	desc, err := b.addMonitor(ctx, m)
	if err != nil {
		return b.tg.sendPlain(ctx, chatID, err.Error())
	}
	return b.tg.sendPlain(ctx, chatID, desc)
}

func (b *bot) watchesCmd(ctx context.Context, chatID int64) error {
	ms, err := b.store.listMonitors(chatID)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		return b.tg.sendPlain(ctx, chatID, "Мониторов нет.\n\n"+watchHelp)
	}
	var sb strings.Builder
	var rows [][]tgButton
	sb.WriteString("Мониторы:\n")
	for _, m := range ms {
		what := monitorKinds[m.Kind]
		if m.Pattern != "" {
			what += " «" + truncRunes(m.Pattern, 40) + "»"
		}
		fmt.Fprintf(&sb, "\n#%d %s — %s, каждые %s", m.ID, truncRunes(m.URL, 80), what, humanInterval(m.Interval))
		toggle := tgButton{Text: fmt.Sprintf("⏸ #%d", m.ID), Data: fmt.Sprintf("mon:%d:pause", m.ID)}
		if m.Status == "paused" {
			sb.WriteString(" ⏸ на паузе")
			toggle = tgButton{Text: fmt.Sprintf("▶️ #%d", m.ID), Data: fmt.Sprintf("mon:%d:resume", m.ID)}
		}
		if m.Fails > 0 {
			fmt.Fprintf(&sb, " ⚠️ сбоев подряд: %d", m.Fails)
		}
		rows = append(rows, []tgButton{toggle, {Text: fmt.Sprintf("✖️ #%d", m.ID), Data: fmt.Sprintf("mon:%d:del", m.ID)}})
	}
	_, err = b.tg.sendButtons(ctx, chatID, sb.String(), rows)
	return err
}

func (b *bot) unwatchCmd(ctx context.Context, chatID int64, arg string) error {
	id, ok := parseID(arg)
	if !ok {
		return b.tg.sendPlain(ctx, chatID, "Какой монитор удалить? /unwatch <номер из /watches>")
	}
	if ok, err := b.store.setMonitorStatus(chatID, id, "deleted"); err != nil || !ok {
		return errors.Join(err, b.tg.sendPlain(ctx, chatID, fmt.Sprintf("Монитора #%d нет.", id)))
	}
	return b.tg.sendPlain(ctx, chatID, fmt.Sprintf("Удалила монитор #%d.", id))
}

// onMonitorButton — mon:<id>:ask|pause|resume|del.
func (b *bot) onMonitorButton(ctx context.Context, cq *tgCallback, id int64, op string) {
	chatID := cq.Message.Chat.ID
	m, err := b.store.getMonitor(chatID, id)
	if err != nil || m == nil {
		b.tg.answerCallback(ctx, cq.ID, "Монитора уже нет")
		return
	}
	switch op {
	case "ask":
		b.tg.answerCallback(ctx, cq.ID, "")
		q := fmt.Sprintf("Сработал монитор #%d (%s):\n%s\n\nРазберись, что изменилось и важно ли это мне.", m.ID, m.URL, m.LastAlert)
		// Обычным путём: спит карта — вопрос ляжет в очередь
		b.enqueue(ctx, chatID, incoming{from: cq.From.ID, text: q})
		return
	case "pause", "resume":
		status, word := "paused", "на паузе"
		if op == "resume" {
			status, word = "active", "снова работает"
		}
		_, _ = b.store.setMonitorStatus(chatID, id, status)
		b.tg.answerCallback(ctx, cq.ID, fmt.Sprintf("Монитор #%d %s", id, word))
	case "del":
		_, _ = b.store.setMonitorStatus(chatID, id, "deleted")
		b.tg.answerCallback(ctx, cq.ID, fmt.Sprintf("Монитор #%d удалён", id))
	}
}

// ── Инструмент ─────────────────────────────────────────────────────────────

func (b *bot) monitorTools() []*tool {
	return []*tool{{
		spec: toolSpecFunc{
			Name: "create_monitor",
			Description: "Следить за страницей и написать владельцу, когда она изменится (change), на ней появится (appears) " +
				"или с неё пропадёт (disappears) текст. Проверки идут без тебя и без видеокарты. Владелец подтверждает кнопкой.",
			Parameters: object(map[string]any{
				"url":              str("адрес страницы"),
				"kind":             map[string]any{"type": "string", "enum": []string{"change", "appears", "disappears"}},
				"text":             str("для appears/disappears — какой текст ищем, как он написан на странице"),
				"interval_minutes": map[string]any{"type": "integer", "description": "как часто проверять, минут (по умолчанию 60)"},
			}, "url", "kind"),
		},
		modes:  everywhere,
		status: func(a json.RawMessage) string { return "🔔 Ставлю монитор: " + argStr(a, "url") },
		confirm: func(_ int64, a json.RawMessage) (string, error) {
			m := b.monitorFromArgs(a)
			if _, err := checkURL(m.URL); err != nil {
				return "", err
			}
			what := monitorKinds[m.Kind]
			if what == "" {
				return "", fmt.Errorf("не знаю вида монитора %q", m.Kind)
			}
			if m.Pattern != "" {
				what += " «" + m.Pattern + "»"
			}
			return fmt.Sprintf("Следить за %s каждые %s: напишу, когда %s", m.URL, humanInterval(m.Interval), what), nil
		},
		run: func(ctx context.Context, chatID int64, a json.RawMessage) (string, error) {
			m := b.monitorFromArgs(a)
			m.ChatID = chatID
			return b.addMonitor(ctx, m)
		},
	}}
}

func (b *bot) monitorFromArgs(a json.RawMessage) *monitor {
	m := &monitor{URL: strings.TrimSpace(argStr(a, "url")), Kind: argStr(a, "kind"),
		Pattern: strings.TrimSpace(argStr(a, "text")), Interval: b.cfg.monitorDefault}
	if n := argInt(a, "interval_minutes"); n > 0 {
		m.Interval = time.Duration(n) * time.Minute
	}
	if m.Interval < b.cfg.monitorMin {
		m.Interval = b.cfg.monitorMin
	}
	if utf8.RuneCountInString(m.Pattern) > 200 {
		m.Pattern = truncRunes(m.Pattern, 200)
	}
	return m
}
