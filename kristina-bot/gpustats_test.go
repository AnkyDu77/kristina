package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	sampleGPU  = "0, NVIDIA A40, P0, 87, 40, 36044, 46068, 61, 210.53, 300.00, 1740\n"
	sampleApps = "2211, llmster, 18534\n3302, media-tts-1, 5120\n3410, media-lipsync-1, 6200\n"
)

func TestFormatGPUStats(t *testing.T) {
	gpus := parseGPUs(sampleGPU + "1, Tesla T4, P8, [N/A], [N/A], 0, 15360, 35, [N/A], 70.00, 300\n")
	if len(gpus) != 2 || gpus[0].Util != 87 || gpus[1].hasUtil || gpus[1].hasPow {
		t.Fatalf("%+v", gpus)
	}
	out := formatGPUStats(gpus, parseApps(sampleApps), time.Now().Add(-3*time.Second))
	for _, want := range []string{
		"🖥 NVIDIA A40 #0 · P0", "▓▓▓▓▓▓▓▓▓░  87%", "35.2 / 45.0 ГБ, свободно 9.8", "61°C · 211 / 300 Вт",
		"мозг (LM Studio) — 18.1 ГБ", "голос (Qwen3-TTS) — 5.0 ГБ", "губы (MuseTalk) — 6.1 ГБ", "Снимок 3 с назад",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("нет %q в:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Tesla T4 #1 · P8\nЗагрузка") {
		t.Error("[N/A] загрузку не рисуем")
	}
}

func TestGPUCmdAsleepDoesNotTouch(t *testing.T) {
	tg, sent := fakeTG(t)
	gpu := &gpuManager{state: "absent", idleTimeout: time.Minute}
	b := newBot(botConfig{allowed: map[int64]bool{1: true}}, tg, nil, newMediaClient("mk"), gpu, testStore(t))
	b.handle(context.Background(), 100, &chatState{}, incoming{from: 1, text: "/gpu"})
	if got := texts(sent()); len(got) != 1 || !strings.Contains(got[0], "будить ради статистики не стану") {
		t.Fatalf("%q", got)
	}
	if gpu.status().State != "absent" {
		t.Fatal("разбудила ради статистики")
	}
}

func TestGPUCmdDoesNotExtendIdle(t *testing.T) {
	var hits atomic.Int32
	modified := time.Now().Add(-2 * time.Second).UTC().Format(http.TimeFormat)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mk" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		hits.Add(1)
		w.Header().Set("Last-Modified", modified)
		switch r.URL.Path {
		case "/gpu/gpu.csv":
			_, _ = w.Write([]byte(sampleGPU))
		case "/gpu/apps.csv":
			_, _ = w.Write([]byte(sampleApps))
		case "/gpu/smi.txt":
			_, _ = w.Write([]byte("+------ NVIDIA-SMI 535.183 ------+\n| 0  NVIDIA A40  On |"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	tg, sent := fakeTG(t)
	idleSince := time.Now().Add(-15 * time.Minute)
	gpu := &gpuManager{state: "running", endpoint: srv.URL, idleTimeout: 20 * time.Minute,
		since: time.Now().Add(-42 * time.Minute), lastActivity: idleSince}
	b := newBot(botConfig{allowed: map[int64]bool{1: true}}, tg, nil, newMediaClient("mk"), gpu, testStore(t))

	cs := &chatState{}
	b.handle(context.Background(), 100, cs, incoming{from: 1, text: "/gpu"})
	b.handle(context.Background(), 100, cs, incoming{from: 1, text: "/gpu full"})

	got := texts(sent())
	if len(got) != 2 || !strings.Contains(got[0], "<pre>") || !strings.Contains(got[0], "мозг (LM Studio)") ||
		!strings.Contains(got[0], "работает 42m0s, авто-стоп через 5m0s") {
		t.Fatalf("сводка: %q", got)
	}
	if !strings.Contains(got[1], "NVIDIA-SMI 535.183") {
		t.Fatalf("полный вывод: %q", got[1])
	}
	if hits.Load() != 3 {
		t.Fatalf("запросов к машине: %d", hits.Load())
	}
	gpu.mu.Lock()
	defer gpu.mu.Unlock()
	if !gpu.lastActivity.Equal(idleSince) || gpu.inFlight != 0 {
		t.Fatal("/gpu не должен продлевать машине жизнь")
	}
}

func TestGPUCmdOldMachine(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler()) // машина поднята до обновления: /gpu/ нет
	defer srv.Close()
	tg, sent := fakeTG(t)
	gpu := &gpuManager{state: "running", endpoint: srv.URL, idleTimeout: time.Minute, lastActivity: time.Now()}
	b := newBot(botConfig{allowed: map[int64]bool{1: true}}, tg, nil, newMediaClient("mk"), gpu, testStore(t))
	b.handle(context.Background(), 100, &chatState{}, incoming{from: 1, text: "/gpu"})
	if got := texts(sent()); len(got) != 1 || !strings.Contains(got[0], "после следующего подъёма") {
		t.Fatalf("%q", got)
	}
}
