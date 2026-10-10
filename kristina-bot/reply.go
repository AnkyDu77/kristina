package main

// Чистка ответа модели: open-weight модели добавляют <think>, а для
// кружка из письменного ответа надо сделать что-то произносимое.

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)
	fenceRe = regexp.MustCompile("(?s)```.*?(```|$)")
	// Сноски вида [текст](url) → текст; голые ссылки выкидываем
	mdLinkRe  = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	urlRe     = regexp.MustCompile(`https?://\S+`)
	mdNoiseRe = regexp.MustCompile("[*_#`>]+")
	spacesRe  = regexp.MustCompile(`\s+`)
)

// stripThinking убирает рассуждения reasoning-моделей. Бывает и так, что
// открывающий тег съел шаблон чата и в ответе остался только </think>.
func stripThinking(s string) string {
	s = thinkRe.ReplaceAllString(s, "")
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	return strings.TrimSpace(s)
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
// Кружок дорог в рендере и ограничен минутой.
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

// truncRunes — не длиннее n рун, с пометкой, что обрезано.
func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
