package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTG — Bot API, который запоминает отправленные сообщения.
func fakeTG(t *testing.T) (*tgAPI, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			mu.Lock()
			sent = append(sent, p["text"].(string))
			mu.Unlock()
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	t.Cleanup(srv.Close)
	old := tgAPIBase
	tgAPIBase = srv.URL
	t.Cleanup(func() { tgAPIBase = old })
	return newTGAPI("x"), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sent...)
	}
}

func TestWakeOnlyForGPUAdmins(t *testing.T) {
	tg, sent := fakeTG(t)
	// без newGPUManager: watchdog и terraform в тесте не нужны
	gpu := &gpuManager{state: "absent", idleTimeout: time.Minute}
	b := newBot(botConfig{
		allowed:   map[int64]bool{1: true, 2: true},
		gpuAdmins: map[int64]bool{1: true},
	}, tg, nil, nil, gpu)

	cs := &chatState{}
	b.handle(context.Background(), 100, cs, incoming{from: 2, text: "/wake"})
	b.handle(context.Background(), 100, cs, incoming{from: 2, text: "/sleep"})

	if st := gpu.status().State; st != "absent" {
		t.Fatalf("не-владелец разбудил карту: %s", st)
	}
	got := sent()
	if len(got) != 2 || !strings.Contains(got[0], "только владелец") || !strings.Contains(got[1], "только владелец") {
		t.Fatalf("ответы: %q", got)
	}
}

func TestMessageDoesNotWakeGPU(t *testing.T) {
	tg, sent := fakeTG(t)
	gpu := &gpuManager{state: "absent", idleTimeout: time.Minute}
	b := newBot(botConfig{
		allowed:   map[int64]bool{1: true},
		gpuAdmins: map[int64]bool{1: true}, // даже владельцу сообщение карту не будит
	}, tg, nil, nil, gpu)

	b.handle(context.Background(), 100, &chatState{}, incoming{from: 1, text: "привет"})

	if st := gpu.status().State; st != "absent" {
		t.Fatalf("сообщение разбудило карту: %s", st)
	}
	got := sent()
	if len(got) != 1 || !strings.Contains(got[0], "/wake") {
		t.Fatalf("ждали подсказку про /wake, получили %q", got)
	}
}
