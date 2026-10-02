package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseReply(t *testing.T) {
	cases := []struct {
		name, raw, speech, text string
	}{
		{"чистый JSON", `{"speech":"Привет.","text":"код"}`, "Привет.", "код"},
		{"в ```json", "```json\n{\"speech\":\"Да.\",\"text\":\"\"}\n```", "Да.", ""},
		{"с размышлениями", "<think>хм, {не json}</think>\n{\"speech\":\"Ок.\",\"text\":\"x\"}", "Ок.", "x"},
		{"только закрывающий think", "рассуждаю...</think>{\"speech\":\"Ага.\",\"text\":\"\"}", "Ага.", ""},
		{"не JSON — всё в текст, речь из него", "Смотри **сюда**:\n```go\nfmt.Println()\n```\nи [доку](https://go.dev).",
			"Смотри сюда: и доку.", "Смотри **сюда**:\n```go\nfmt.Println()\n```\nи [доку](https://go.dev)."},
		{"пустая речь — берём из текста", `{"speech":"","text":"Всё готово."}`, "Всё готово.", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := parseReply(c.raw, 600)
			if r.Speech != c.speech || r.Text != c.text {
				t.Fatalf("got speech=%q text=%q\nwant speech=%q text=%q", r.Speech, r.Text, c.speech, c.text)
			}
		})
	}
}

func TestClampSpeech(t *testing.T) {
	s := "Первое предложение. Второе предложение длиннее! Третье"
	if got := clampSpeech(s, 50); got != "Первое предложение. Второе предложение длиннее!" {
		t.Fatalf("по границе предложения: %q", got)
	}
	long := strings.Repeat("слово ", 50)
	got := clampSpeech(long, 30)
	if utf8.RuneCountInString(got) > 31 || !strings.HasSuffix(got, "…") {
		t.Fatalf("без точек — по пробелу с многоточием: %q", got)
	}
	if got := clampSpeech("коротко", 600); got != "коротко" {
		t.Fatalf("короткое не трогаем: %q", got)
	}
}
