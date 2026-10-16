//go:build live

package main

// Живая проверка инструментов без GPU и телеграма: настоящий Parallel и
// настоящие страницы. Запуск: go test -tags live -run Live -v
// Гонять с VPS бота — с мака под Happ в TUN-режиме сеть ведёт себя иначе.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestLiveSandbox — настоящая песочница: нужен докер и собранный образ
// (docker compose --profile sandbox build sandbox). На VPS бота:
// go test -tags live -run LiveSandbox -v
func TestLiveSandbox(t *testing.T) {
	// Тем же путём, что в бою: openWorkspace отдаёт папку пользователю песочницы
	ws, err := openWorkspace(filepath.Join(t.TempDir(), "workspace"))
	if err != nil {
		t.Fatal(err)
	}
	sb := sandboxFromEnv(ws.dir, "none")
	if sb == nil {
		t.Skip("докера или образа песочницы нет")
	}
	ds := sb.(*dockerSandbox)
	defer ds.reset(context.Background())
	_ = ds.reset(context.Background())
	ctx := context.Background()
	for _, c := range []struct {
		cmd, want string
		code      int
	}{
		{"id -u && python3 -c 'print(6*7)' && go version", "1000\n42", 0},
		{"echo hi > /workspace/f && cat /workspace/f", "hi", 0},
		{"touch /etc/x", "Read-only", 1},
		{"curl -sS -m 5 https://go.dev > /dev/null", "", 6}, // сети нет: не резолвится
	} {
		out, code, err := ds.run(ctx, c.cmd, 20*time.Second)
		if err != nil || !strings.Contains(out, c.want) || code != c.code {
			t.Errorf("%q → код %d, %v:\n%s", c.cmd, code, err, out)
		}
	}
	start := time.Now()
	if _, code, _ := ds.run(ctx, "sleep 30", 2*time.Second); code != 137 || time.Since(start) > 10*time.Second {
		t.Errorf("потолок времени: код %d за %s", code, time.Since(start))
	}
}

// TestLiveHiggsfield — только бесплатное: смета, параметры модели, баланс.
// Нужен залогиненный CLI (higgsfield auth login) в PATH.
func TestLiveHiggsfield(t *testing.T) {
	h := higgsfieldFromEnv()
	if h == nil {
		t.Skip("нет CLI higgsfield")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	credits, plan, err := h.balance(ctx)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	t.Logf("баланс %s кр, тариф %s", fmtCr(credits), plan)
	r := hfRequest{Model: hfSoulModel, Prompt: "test", Params: map[string]string{"aspect_ratio": "3:4"}}
	h.soulID = "2a978ec2-087e-4a81-bbdc-8f1d5fb46847"
	h.prepare(&r)
	c, err := h.cost(ctx, r)
	if err != nil {
		t.Fatalf("cost: %v", err)
	}
	t.Logf("смета Soul: %s кр", fmtCr(c))
	if c <= 0 {
		t.Fatalf("смета %v", c)
	}
	info, err := h.modelInfo(ctx, "kling_o3_flf")
	if err != nil || !strings.Contains(info, "start_image") {
		t.Fatalf("model info: %v %q", err, truncRunes(info, 300))
	}
	if _, err := h.modelInfo(ctx, "no_such_model_x"); err == nil {
		t.Fatal("несуществующая модель должна дать ошибку")
	}
	if _, err := h.get(ctx, "16c1843b-4516-488f-aff8-0095a0c7c324"); err != nil {
		t.Fatalf("get: %v", err)
	}
}
