//go:build live

package main

// Живая проверка инструментов без GPU и телеграма: настоящий Parallel и
// настоящие страницы. Запуск: go test -tags live -run Live -v
// Гонять с VPS бота — с мака под Happ в TUN-режиме сеть ведёт себя иначе.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestLiveTools(t *testing.T) {
	b := newBot(botConfig{}, nil, nil, nil, nil, testStore(t))
	ctx := context.Background()

	res, err := b.toolSearch(ctx, 1, json.RawMessage(`{"objective":"актуальная версия языка Go","search_queries":["go release history","golang latest version"]}`))
	if err != nil {
		t.Fatalf("web_search: %v", err)
	}
	t.Logf("web_search:\n%s", truncRunes(res, 1500))

	for _, u := range []string{
		"https://go.dev/doc/devel/release", // читаем сами
		"https://lmstudio.ai/docs/app",     // скорее всего, рисуется JS — уйдёт в Parallel
	} {
		page, err := b.toolReadPage(ctx, 1, json.RawMessage(`{"url":"`+u+`","objective":"основное содержание"}`))
		if err != nil {
			t.Errorf("read_page %s: %v", u, err)
			continue
		}
		via := "сами"
		if strings.HasPrefix(page, "Выдержки") {
			via = "Parallel"
		}
		t.Logf("read_page %s (%s), %d рун:\n%s", u, via, len([]rune(page)), truncRunes(page, 800))
	}
}
