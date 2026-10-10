package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

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
