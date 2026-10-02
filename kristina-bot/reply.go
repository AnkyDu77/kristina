package main

// Разбор ответа модели на две части. Модель не обязана слушаться:
// open-weight модели оборачивают JSON в ```json, добавляют <think>,
// а иногда отвечают просто текстом. Всё это — не повод молчать:
// из любого ответа достаём, что сказать вслух и что написать.

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

type reply struct {
	Speech string `json:"speech"`
	Text   string `json:"text"`
}

var (
	thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)
	fenceRe = regexp.MustCompile("(?s)```.*?(```|$)")
	// Сноски вида [текст](url) → текст; голые ссылки выкидываем
	mdLinkRe  = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	urlRe     = regexp.MustCompile(`https?://\S+`)
	mdNoiseRe = regexp.MustCompile("[*_#`>]+")
	spacesRe  = regexp.MustCompile(`\s+`)
)

func parseReply(raw string, maxSpeech int) reply {
	s := stripThinking(raw)

	var r reply
	if !decodeReply(s, &r) {
		// Не JSON — весь ответ пишем текстом, а вслух говорим его начало
		r = reply{Text: s}
	}
	r.Speech = strings.TrimSpace(r.Speech)
	r.Text = strings.TrimSpace(r.Text)
	if r.Speech == "" {
		r.Speech = speechFrom(r.Text)
	}
	r.Speech = clampSpeech(r.Speech, maxSpeech)
	// Всё сказанное уместилось в кружок — дублировать текстом незачем
	if r.Text == r.Speech {
		r.Text = ""
	}
	return r
}

// stripThinking убирает рассуждения reasoning-моделей. Бывает и так, что
// открывающий тег съел шаблон чата и в ответе остался только </think>.
func stripThinking(s string) string {
	s = thinkRe.ReplaceAllString(s, "")
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	return strings.TrimSpace(s)
}

func decodeReply(s string, r *reply) bool {
	candidates := []string{s}
	// ```json {...} ``` или JSON посреди болтовни — берём от первой {
	// до последней }
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		candidates = append(candidates, s[i:j+1])
	}
	for _, c := range candidates {
		var tmp reply
		if json.Unmarshal([]byte(c), &tmp) == nil && (tmp.Speech != "" || tmp.Text != "") {
			*r = tmp
			return true
		}
	}
	return false
}

// speechFrom делает из письменного текста что-то произносимое: без кода,
// ссылок и разметки.
func speechFrom(text string) string {
	s := fenceRe.ReplaceAllString(text, " ")
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = urlRe.ReplaceAllString(s, "")
	s = mdNoiseRe.ReplaceAllString(s, "")
	return strings.TrimSpace(spacesRe.ReplaceAllString(s, " "))
}

// clampSpeech режет речь по границе предложения, не длиннее max рун.
// Кружок дорог в рендере и ограничен минутой, поэтому лишнее — в text.
func clampSpeech(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)[:max]
	cut := string(r)
	if i := strings.LastIndexAny(cut, ".!?…"); i > 0 {
		_, size := utf8.DecodeRuneInString(cut[i:])
		return strings.TrimSpace(cut[:i+size])
	}
	// Ни одного конца предложения — режем по последнему пробелу
	if i := strings.LastIndex(cut, " "); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}
