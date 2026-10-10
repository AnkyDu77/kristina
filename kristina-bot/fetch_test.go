package main

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"8.8.8.8":         true,
		"2a00:1450::1":    true,
		"127.0.0.1":       false,
		"10.1.2.3":        false,
		"192.168.0.1":     false,
		"172.16.5.5":      false,
		"169.254.169.254": false, // метаданные облака
		"100.64.0.1":      false, // CGNAT
		"0.0.0.0":         false,
		"::1":             false,
		"::ffff:10.0.0.1": false, // IPv4 в IPv6-обёртке
		"fd00::1":         false,
		"fe80::1":         false,
	} {
		if got := publicAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v, ждали %v", addr, got, want)
		}
	}
}

func TestFetchRefusesLocalhost(t *testing.T) {
	for _, u := range []string{"http://127.0.0.1:1/", "http://localhost:8080/", "http://[::1]/"} {
		_, err := fetchPage(context.Background(), newFetchClient(), u)
		if !errors.Is(err, errNotPublic) {
			t.Errorf("%s: %v", u, err)
		}
	}
	if _, err := checkURL("file:///etc/passwd"); err == nil {
		t.Error("file:// пропущен")
	}
	if _, err := checkURL("https://user:pass@example.com/"); err == nil {
		t.Error("адрес с паролем пропущен")
	}
}

func TestHTMLText(t *testing.T) {
	src := `<html><head><title>Релизы Go</title><style>.x{}</style><script>alert(1)</script></head>
<body><nav><a href="/">Главная</a> | <a>Доки</a></nav>
<h1>Release   History</h1><p>Go 1.27 вышел.<br>Ура.</p>
<ul><li>один</li><li>два</li></ul><img src="x.png"/><footer>© Google</footer></body></html>`
	title, text := htmlText(strings.NewReader(src))
	if title != "Релизы Go" {
		t.Fatalf("title: %q", title)
	}
	want := "# Release History\n\nGo 1.27 вышел.\nУра.\n\n- один\n\n- два"
	if text != want {
		t.Fatalf("text:\n%q\nwant:\n%q", text, want)
	}

	// Незакрытые <nav> внутри закрытого <div> (как у go.dev): токенайзер со
	// счётчиком тегов съедал из-за них всю страницу; ссылки не слипаются
	_, text = htmlText(strings.NewReader(`<body><div class="drawer"><nav><a>Доки</a><a>Блог</a>` +
		`<nav><a>ещё</a></div><main><h2>go1.27.0</h2><p><a>Скачать</a><a>Заметки</a></p></main>`))
	if text != "## go1.27.0\n\nСкачать Заметки" {
		t.Fatalf("незакрытый nav: %q", text)
	}
}

func TestSSEResponse(t *testing.T) {
	stream := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n" +
		"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"ok\":true}}\n\n"
	got, err := sseResponse(strings.NewReader(stream), 7)
	if err != nil || !strings.Contains(string(got), `"ok":true`) {
		t.Fatalf("%s, %v", got, err)
	}
	if _, err := sseResponse(strings.NewReader(stream), 8); err == nil {
		t.Fatal("ответа на 8 в потоке нет")
	}
}

func TestHistoryStartsWithUser(t *testing.T) {
	st := testStore(t)
	for i, m := range []chatMsg{{"user", "q1", nil, ""}, {"assistant", "a1", nil, ""}, {"user", "q2", nil, ""}, {"assistant", "a2", nil, ""}} {
		if err := st.addMessage(100, m.Role, m.Content, i); err != nil {
			t.Fatal(err)
		}
	}
	// LIMIT 3 начался бы с a1 — его отрезаем
	h, err := st.history(100, 3)
	if err != nil || len(h) != 2 || h[0].Content != "q2" || h[1].Content != "a2" {
		t.Fatalf("%+v, %v", h, err)
	}
	if err := st.resetHistory(100); err != nil {
		t.Fatal(err)
	}
	if h, _ := st.history(100, 10); len(h) != 0 {
		t.Fatalf("после /reset: %+v", h)
	}
	var total int
	_ = st.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&total)
	if total != 4 {
		t.Fatalf("soft delete: строк %d", total)
	}
}
