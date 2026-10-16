package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSlotAt(t *testing.T) {
	for h, want := range map[int]string{0: "night", 5: "night", 6: "morning", 10: "morning", 11: "day", 16: "day", 17: "evening", 22: "evening", 23: "night"} {
		if got := slotAt(time.Date(2026, 10, 16, h, 59, 0, 0, time.UTC)).Key; got != want {
			t.Errorf("%02d:59 → %s, ждали %s", h, got, want)
		}
	}
	if s, ok := slotByKey("Вечер"); !ok || s.Key != "evening" {
		t.Fatalf("слот по-русски: %+v %v", s, ok)
	}
}

func TestSceneMakeRequest(t *testing.T) {
	f := newFakeHF("")
	f.cost = "6.25"
	b, _ := hfBot(t, "http://unused", f, 60, 40)
	tl := b.toolByName("circle_scene_make")
	if tl == nil || tl.confirm == nil {
		t.Fatal("circle_scene_make должен быть и с кнопкой")
	}
	args := json.RawMessage(`{"photo":"00000000-0000-0000-0000-0000000000aa","slot":"morning","motion":"thin steam rises from the tea cup."}`)
	summary, err := tl.confirm(100, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "Сцена для кружков — утро") || !strings.Contains(summary, "kling_o3_flf — 6.25 кр") {
		t.Fatalf("кнопка: %q", summary)
	}
	if _, err := tl.run(context.Background(), 100, args); err != nil {
		t.Fatal(err)
	}
	cr := f.creates()
	if len(cr) != 1 {
		t.Fatalf("создано: %d", len(cr))
	}
	got := strings.Join(cr[0], " ")
	for _, want := range []string{"--start-image=00000000-0000-0000-0000-0000000000aa", "--end-image=00000000-0000-0000-0000-0000000000aa",
		"--aspect_ratio=1:1", "--multi_shots=false", "--duration=5", "--sound=off", "steam rises from the tea cup", "mouth closed"} {
		if !strings.Contains(got, want) {
			t.Errorf("в запросе нет %q:\n%s", want, got)
		}
	}
	jobs, _ := b.store.pendingHFJobs()
	if len(jobs) != 1 || !strings.HasPrefix(jobs[0].Name, "scenes/morning-") {
		t.Fatalf("куда ляжет петля: %+v", jobs)
	}
	if _, err := tl.confirm(100, json.RawMessage(`{"photo":"x.png","slot":"полдник"}`)); err == nil {
		t.Fatal("неизвестный слот должен дать ошибку")
	}
}

// fakeMedia — TTS и lip-sync на «GPU»: запоминает, какой аватар загрузили.
func fakeMedia(t *testing.T) (url string, avatars func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	known := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/tts/speak":
			_, _ = w.Write([]byte("RIFFwav"))
		case strings.HasPrefix(r.URL.Path, "/lipsync/avatars/") && r.Method == http.MethodGet:
			mu.Lock()
			ok := known[strings.TrimPrefix(r.URL.Path, "/lipsync/avatars/")]
			mu.Unlock()
			if !ok {
				http.NotFound(w, r)
			}
		case r.URL.Path == "/lipsync/avatars":
			_ = r.ParseMultipartForm(1 << 20)
			_, fh, _ := r.FormFile("file")
			mu.Lock()
			known[r.FormValue("avatar_id")] = true
			got = append(got, fh.Filename)
			mu.Unlock()
		case r.URL.Path == "/lipsync/render":
			_, _ = w.Write([]byte("mp4"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func TestCircleUsesSceneOfCurrentSlot(t *testing.T) {
	mediaURL, avatars := fakeMedia(t)
	tg, _ := fakeTG(t)
	dir := t.TempDir()
	avatar := filepath.Join(dir, "idle.mp4")
	_ = os.WriteFile(avatar, []byte("idle"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "voice"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "voice", "ref.wav"), []byte("wav"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "voice", "ref.txt"), []byte("привет"), 0o644)
	b := newBot(botConfig{allowed: map[int64]bool{1: true}, staticBase: mediaURL, avatarPath: avatar, dataDir: dir,
		maxSpeech: 600, circleSide: 512, tz: time.UTC}, tg, nil, newMediaClient("k"), nil, testStore(t))
	ws, err := openWorkspace(filepath.Join(dir, "ws"))
	if err != nil {
		t.Fatal(err)
	}
	b.attach(ws, nil, nil)

	// Сцены нет — запасной аватар
	if _, _, label, err := b.circleAvatar(time.Now()); err != nil || !strings.Contains(label, "запасной") {
		t.Fatalf("без сцены: %q %v", label, err)
	}

	slot := slotAt(b.nowLocal()).Key
	_ = ws.writeBytes("scenes/now.mp4", []byte("scene-video"))
	set := b.toolByName("circle_scene_set")
	if res, err := set.run(context.Background(), 100, json.RawMessage(`{"slot":"`+slot+`","scene":"/workspace/scenes/now.mp4","title":"тест"}`)); err != nil {
		t.Fatalf("circle_scene_set: %v", err)
	} else if !strings.Contains(res, "scenes/now.mp4") {
		t.Fatalf("ответ: %q", res)
	}
	if res, _ := b.toolByName("circle_scenes").run(context.Background(), 100, nil); !strings.Contains(res, "«тест»") {
		t.Fatalf("circle_scenes: %q", res)
	}

	// Обычный кружок идёт на сцене текущего слота
	mp4, err := b.circle(context.Background(), mediaURL, "привет", voiceRef{WAV: []byte("w"), Text: "т"})
	if err != nil || string(mp4) != "mp4" {
		t.Fatalf("кружок: %q %v", mp4, err)
	}
	if got := avatars(); len(got) != 1 || got[0] != "now.mp4" {
		t.Fatalf("загруженные аватары: %v", got)
	}

	// Пробный кружок на конкретной сцене — владельцу
	_ = ws.writeBytes("scenes/try.mp4", []byte("other-scene"))
	if _, err := b.toolByName("circle_preview").run(context.Background(), 100, json.RawMessage(`{"scene":"scenes/try.mp4","text":"Доброе утро!"}`)); err != nil {
		t.Fatalf("circle_preview: %v", err)
	}
	if got := avatars(); len(got) != 2 || got[1] != "try.mp4" {
		t.Fatalf("аватар пробного кружка: %v", got)
	}

	// Файл сцены пропал — снова запасной аватар, а не ошибка
	_ = os.Remove(filepath.Join(dir, "ws", "scenes", "now.mp4"))
	if _, _, label, err := b.circleAvatar(b.nowLocal()); err != nil || !strings.Contains(label, "запасной") {
		t.Fatalf("сцена пропала: %q %v", label, err)
	}
	// default — вернуть слот на запасной
	if _, err := set.run(context.Background(), 100, json.RawMessage(`{"slot":"`+slot+`","scene":"default"}`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.store.activeScene(slot); ok {
		t.Fatal("после default сцены быть не должно")
	}
}
