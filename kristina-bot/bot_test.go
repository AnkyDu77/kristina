package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// tgSent — что бот отправил или переписал в телеграме.
type tgSent struct {
	method  string
	text    string
	msgID   int64
	buttons []string // callback_data
}

// fakeTG — Bot API, который запоминает отправленные сообщения и выдаёт
// им message_id по порядку.
func fakeTG(t *testing.T) (*tgAPI, func() []tgSent) {
	t.Helper()
	var mu sync.Mutex
	var sent []tgSent
	var nextID int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch {
		case strings.HasPrefix(r.URL.Path, "/file/"):
			// скачивание присланного файла: содержимое = путь, чтобы тест его узнал
			_, _ = w.Write([]byte("содержимое " + strings.TrimPrefix(r.URL.Path, "/file/botx/")))
			return
		case method == "getFile":
			var p struct {
				FileID string `json:"file_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": "docs/" + p.FileID}})
			return
		case method == "sendPhoto" || method == "sendDocument":
			_ = r.ParseMultipartForm(1 << 20)
			name := ""
			for _, fh := range r.MultipartForm.File {
				name = fh[0].Filename
			}
			mu.Lock()
			sent = append(sent, tgSent{method: method, text: r.FormValue("caption") + "|" + name})
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
			return
		}
		if method == "sendMessage" || method == "editMessageText" {
			var p struct {
				Text        string `json:"text"`
				MessageID   int64  `json:"message_id"`
				ReplyMarkup struct {
					Keyboard [][]tgButton `json:"inline_keyboard"`
				} `json:"reply_markup"`
			}
			_ = json.NewDecoder(r.Body).Decode(&p)
			mu.Lock()
			s := tgSent{method: method, text: p.Text, msgID: p.MessageID}
			if method == "sendMessage" {
				nextID++
				s.msgID = nextID
			}
			for _, row := range p.ReplyMarkup.Keyboard {
				for _, b := range row {
					s.buttons = append(s.buttons, b.Data)
				}
			}
			sent = append(sent, s)
			id := s.msgID
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": id}})
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	t.Cleanup(srv.Close)
	old := tgAPIBase
	tgAPIBase = srv.URL
	t.Cleanup(func() { tgAPIBase = old })
	return newTGAPI("x"), func() []tgSent {
		mu.Lock()
		defer mu.Unlock()
		return append([]tgSent(nil), sent...)
	}
}

func texts(sent []tgSent) []string {
	var out []string
	for _, s := range sent {
		out = append(out, s.text)
	}
	return out
}

func testStore(t *testing.T) *store {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.db.Close() })
	return st
}

func TestWakeOnlyForGPUAdmins(t *testing.T) {
	tg, sent := fakeTG(t)
	// без newGPUManager: watchdog и terraform в тесте не нужны
	gpu := &gpuManager{state: "absent", idleTimeout: time.Minute}
	b := newBot(botConfig{
		allowed:   map[int64]bool{1: true, 2: true},
		gpuAdmins: map[int64]bool{1: true},
	}, tg, nil, nil, gpu, testStore(t))

	cs := &chatState{}
	b.handle(context.Background(), 100, cs, incoming{from: 2, text: "/wake"})
	b.handle(context.Background(), 100, cs, incoming{from: 2, text: "/sleep"})

	if st := gpu.status().State; st != "absent" {
		t.Fatalf("не-владелец разбудил карту: %s", st)
	}
	got := texts(sent())
	if len(got) != 2 || !strings.Contains(got[0], "только владелец") || !strings.Contains(got[1], "только владелец") {
		t.Fatalf("ответы: %q", got)
	}
}

func TestAsleepMessagesGoToInbox(t *testing.T) {
	tg, sent := fakeTG(t)
	gpu := &gpuManager{state: "absent", idleTimeout: time.Minute}
	st := testStore(t)
	b := newBot(botConfig{
		allowed:   map[int64]bool{1: true, 2: true},
		gpuAdmins: map[int64]bool{1: true}, // даже владельцу сообщение карту не будит
	}, tg, nil, nil, gpu, st)

	cs := &chatState{}
	b.handle(context.Background(), 100, cs, incoming{from: 1, text: "привет"})
	b.handle(context.Background(), 100, cs, incoming{from: 1, text: "и ещё вопрос"})
	b.handle(context.Background(), 200, cs, incoming{from: 2, text: "а я не владелец"})

	if s := gpu.status().State; s != "absent" {
		t.Fatalf("сообщение разбудило карту: %s", s)
	}
	got := sent()
	if len(got) != 3 {
		t.Fatalf("ответы: %q", texts(got))
	}
	if !strings.Contains(got[0].text, "записала") || len(got[0].buttons) != 1 || got[0].buttons[0] != "wake" {
		t.Fatalf("первый отложенный — с кнопкой «разбудить»: %+v", got[0])
	}
	if !strings.Contains(got[1].text, "в очереди: 2") || got[1].buttons != nil {
		t.Fatalf("второй — коротко и без кнопки: %+v", got[1])
	}
	if got[2].buttons != nil || !strings.Contains(got[2].text, "владелец") {
		t.Fatalf("не-владельцу кнопку не показываем: %+v", got[2])
	}
	if n := st.countInbox(100); n != 2 {
		t.Fatalf("в очереди %d, ждали 2", n)
	}
}

func TestCommandsWorkWithoutGPU(t *testing.T) {
	tg, sent := fakeTG(t)
	gpu := &gpuManager{state: "absent", idleTimeout: time.Minute}
	st := testStore(t)
	b := newBot(botConfig{allowed: map[int64]bool{1: true}}, tg, nil, nil, gpu, st)

	cs := &chatState{}
	for _, cmd := range []string{"/remember пишет на Go", "/memory", "/forget 1", "/memory", "/circles off", "/status"} {
		b.handle(context.Background(), 100, cs, incoming{from: 1, text: cmd})
	}
	got := texts(sent())
	if len(got) != 6 {
		t.Fatalf("ответы: %q", got)
	}
	if !strings.Contains(got[1], "#1 пишет на Go") || !strings.Contains(got[2], "Забыла #1") || !strings.Contains(got[3], "ничего о тебе не помню") {
		t.Fatalf("память: %q", got)
	}
	if !cs.noCircl || st.getKV(circlesKey(100)) != "off" || !strings.Contains(got[5], "Кружки: выключены") {
		t.Fatalf("кружки: %v %q", cs.noCircl, got[5])
	}
	if st.countInbox(100) != 0 {
		t.Fatal("команды в очередь не попадают")
	}
}

func TestCircleFallsBackToVideoWhenForbidden(t *testing.T) {
	var mu sync.Mutex
	var calls []string // метод + подпись
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		_ = r.ParseMultipartForm(1 << 20)
		mu.Lock()
		calls = append(calls, method+"|"+r.FormValue("caption"))
		mu.Unlock()
		if method == "sendVideoNote" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: VOICE_MESSAGES_FORBIDDEN"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()
	old := tgAPIBase
	tgAPIBase = srv.URL
	defer func() { tgAPIBase = old }()

	b := newBot(botConfig{circleSide: 512}, newTGAPI("x"), nil, nil, nil, nil)
	for i := 0; i < 2; i++ {
		if err := b.sendCircle(context.Background(), 100, []byte("mp4")); err != nil {
			t.Fatalf("попытка %d: %v", i, err)
		}
	}

	if len(calls) != 4 || !strings.HasPrefix(calls[1], "sendVideo|") || !strings.HasPrefix(calls[3], "sendVideo|") {
		t.Fatalf("ждали кружок → видео дважды, получили %q", calls)
	}
	if !strings.Contains(calls[1], "Конфиденциальность") {
		t.Fatalf("первое видео — с подсказкой: %q", calls[1])
	}
	if calls[3] != "sendVideo|" {
		t.Fatalf("второе — уже без подсказки: %q", calls[3])
	}
}
