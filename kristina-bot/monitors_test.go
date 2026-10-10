package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePage — страница, содержимое и код ответа которой меняет тест.
func fakePage(t *testing.T) (url string, set func(status int, body string)) {
	var mu sync.Mutex
	status, body := 200, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte("<html><body>" + body + "</body></html>"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/item", func(s int, b string) {
		mu.Lock()
		status, body = s, b
		mu.Unlock()
	}
}

func monitorBot(t *testing.T) *bot {
	b, _ := agentBot(t, "http://unused")
	b.fetch = &http.Client{Timeout: 5 * time.Second} // httptest — на localhost, а боевой клиент его не пустит
	return b
}

func TestMonitorAppearsOnlyOnTransition(t *testing.T) {
	b := monitorBot(t)
	url, set := fakePage(t)
	set(200, "<p>Нет в наличии</p>")

	desc, err := b.addMonitor(context.Background(), &monitor{ChatID: 100, URL: url, Kind: "appears", Pattern: "в  НАЛИЧИИ  есть", Interval: time.Hour})
	if err != nil || !strings.Contains(desc, "Монитор #1") {
		t.Fatalf("%q, %v", desc, err)
	}
	m, _ := b.store.getMonitor(100, 1)
	if m.Interval != time.Hour || m.State != "0" {
		t.Fatalf("начальное состояние: %+v", m)
	}

	if a := b.checkMonitor(context.Background(), m); a != "" {
		t.Fatalf("ничего не поменялось, а алерт: %q", a)
	}
	set(200, "<p>В наличии есть!</p>")
	if a := b.checkMonitor(context.Background(), m); !strings.Contains(a, "появилось") {
		t.Fatalf("текст появился (регистр и пробелы не важны): %q", a)
	}
	if a := b.checkMonitor(context.Background(), m); a != "" {
		t.Fatalf("одно и то же событие дважды: %q", a)
	}
	m, _ = b.store.getMonitor(100, 1)
	if !strings.Contains(m.LastAlert, "появилось") || m.NextRun.Before(time.Now().Add(50*time.Minute)) {
		t.Fatalf("сохранено: %+v", m)
	}
}

func TestMonitorChangeShowsDiff(t *testing.T) {
	b := monitorBot(t)
	url, set := fakePage(t)
	set(200, "<p>Go 1.26</p><p>Скачать</p>")
	if _, err := b.addMonitor(context.Background(), &monitor{ChatID: 100, URL: url, Kind: "change", Interval: time.Hour}); err != nil {
		t.Fatal(err)
	}
	m, _ := b.store.getMonitor(100, 1)
	set(200, "<p>Go 1.27</p><p>Go 1.26</p><p>Скачать</p>")
	a := b.checkMonitor(context.Background(), m)
	if !strings.Contains(a, "изменилась") || !strings.Contains(a, "• Go 1.27") || strings.Contains(a, "Скачать") {
		t.Fatalf("алерт: %q", a)
	}
}

func TestMonitorBackoffAndRecovery(t *testing.T) {
	b := monitorBot(t)
	url, set := fakePage(t)
	set(200, "<p>ok</p>")
	if _, err := b.addMonitor(context.Background(), &monitor{ChatID: 100, URL: url, Kind: "change", Interval: 10 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	m, _ := b.store.getMonitor(100, 1)

	set(500, "")
	var alerts []string
	for i := 0; i < 4; i++ {
		if a := b.checkMonitor(context.Background(), m); a != "" {
			alerts = append(alerts, a)
		}
	}
	if len(alerts) != 1 || !strings.Contains(alerts[0], "3 раза подряд") {
		t.Fatalf("про сбой — один раз, на третьем: %q", alerts)
	}
	if left := time.Until(m.NextRun); left < 70*time.Minute {
		t.Fatalf("после 4 сбоев интервал растёт (10m·2³): до следующей %s", left)
	}
	set(200, "<p>ok</p>")
	if a := b.checkMonitor(context.Background(), m); a != "✅ Страница снова открывается." {
		t.Fatalf("восстановление: %q", a)
	}
	if m.Fails != 0 || time.Until(m.NextRun) > 11*time.Minute {
		t.Fatalf("после восстановления — обычный интервал: %+v", m)
	}
}

func TestMonitorRejectsBadInput(t *testing.T) {
	b, _ := agentBot(t, "http://unused") // боевой fetch-клиент
	for _, m := range []*monitor{
		{URL: "http://127.0.0.1:8080/", Kind: "change"},
		{URL: "https://example.com", Kind: "appears"}, // без текста
		{URL: "ftp://example.com", Kind: "change"},
	} {
		m.ChatID, m.Interval = 100, time.Hour
		if _, err := b.addMonitor(context.Background(), m); err == nil {
			t.Errorf("%+v: должен быть отказ", m)
		}
	}
	if ms, _ := b.store.listMonitors(100); len(ms) != 0 {
		t.Fatalf("битые мониторы не заводятся: %+v", ms)
	}
}

func TestParseWatch(t *testing.T) {
	for _, c := range []struct {
		arg, url, kind, pattern string
		interval                time.Duration
	}{
		{"https://a.ru/x", "https://a.ru/x", "change", "", time.Hour},
		{`https://a.ru/x 30m "В наличии"`, "https://a.ru/x", "appears", "В наличии", 30 * time.Minute},
		{`https://a.ru/x !«Нет в наличии» 2ч`, "https://a.ru/x", "disappears", "Нет в наличии", 2 * time.Hour},
	} {
		m, err := parseWatch(c.arg, time.Hour)
		if err != nil || m.URL != c.url || m.Kind != c.kind || m.Pattern != c.pattern || m.Interval != c.interval {
			t.Errorf("%q → %+v, %v", c.arg, m, err)
		}
	}
	if _, err := parseWatch("https://a.ru лишнее", time.Hour); err == nil {
		t.Error("лишнее слово пропущено")
	}
}

func TestWatchCommandsWithoutGPU(t *testing.T) {
	b := monitorBot(t)
	b.cfg.staticBase = ""
	b.gpu = &gpuManager{state: "absent", idleTimeout: time.Minute}
	url, set := fakePage(t)
	set(200, "<p>привет</p>")
	cs := &chatState{}
	b.handle(context.Background(), 100, cs, incoming{from: 1, text: "/watch " + url + " 5m"})
	m, _ := b.store.getMonitor(100, 1)
	if m == nil || m.Interval != 10*time.Minute {
		t.Fatalf("интервал не реже минимального: %+v", m)
	}
	b.handle(context.Background(), 100, cs, incoming{from: 1, text: "/unwatch 1"})
	if m, _ := b.store.getMonitor(100, 1); m != nil {
		t.Fatal("/unwatch не сработал")
	}
	if b.store.countInbox(100) != 0 {
		t.Fatal("команды мониторов не должны попадать в очередь к спящей карте")
	}
}
