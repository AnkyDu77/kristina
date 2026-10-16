package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRecallTerms(t *testing.T) {
	got := recallTerms("Задержки рейсов, GPU и Postgres — да, ещё раз памяти!")
	// «да», «и» короче трёх — выпадают; трёхбуквенные не режутся
	want := []string{"задерж", "рейс", "gpu", "postgres", "ещё", "раз", "памя"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recallTerms = %q, want %q", got, want)
	}
	if got := recallTerms("да и в Go"); len(got) != 0 {
		t.Fatalf("короткие слова должны выпасть: %q", got)
	}
}

func TestExcerptAround(t *testing.T) {
	body := strings.Repeat("а", 1000) + " Нашла ЗАДЕРЖКУ рейса " + strings.Repeat("б", 1000)
	ex := excerptAround(body, []string{"задерж"}, 100)
	if !strings.Contains(ex, "ЗАДЕРЖКУ") || !strings.HasPrefix(ex, "…") || !strings.HasSuffix(ex, "…") {
		t.Fatalf("выдержка мимо совпадения: %q", ex)
	}
	if short := "коротко"; excerptAround(short, []string{"кор"}, 100) != short {
		t.Fatal("короткий текст должен вернуться целиком")
	}
}

func TestRecallFindsOldMessagesAndTasks(t *testing.T) {
	st := testStore(t)
	add := func(chat int64, role, text string) {
		t.Helper()
		if err := st.addMessage(chat, role, text, 0); err != nil {
			t.Fatal(err)
		}
	}
	add(1, "user", "Как мы решили с задержкой рейсов в aiplace?")
	add(1, "assistant", "Считаем риск задержки по истории рейса за 90 дней.")
	add(2, "user", "Задержка в чужом чате")

	hits, err := st.recall(1, "задержки", recallOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("морфология: ждали 2 реплики чата 1, got %+v", hits)
	}
	for _, h := range hits {
		if h.At.IsZero() || h.Kind == "task" {
			t.Fatalf("плохое попадание: %+v", h)
		}
	}

	// Видимое окно истории не дублируем
	from := st.visibleFrom(1, 1)
	hits, _ = st.recall(1, "задержки", recallOpts{skipFrom: from})
	if len(hits) != 1 || hits[0].Kind != "user" {
		t.Fatalf("окно истории не пропущено: %+v", hits)
	}

	// Задача: цель сразу, журнал — по мере работы
	id, err := st.createTask(1, "Сравнить sqlite и Postgres")
	if err != nil {
		t.Fatal(err)
	}
	task, _ := st.getTask(id)
	task.Journal = "Сделано: замерила pgvector на ста тысячах векторов"
	if err := st.saveTaskProgress(task); err != nil {
		t.Fatal(err)
	}
	hits, _ = st.recall(1, "векторы pgvector", recallOpts{})
	if len(hits) != 1 || hits[0].Kind != "task" || hits[0].ID != id || !strings.Contains(hits[0].Excerpt, "Postgres") {
		t.Fatalf("журнал задачи не находится: %+v", hits)
	}
	if hits, _ = st.recall(1, "pgvector", recallOpts{skipTask: id}); len(hits) != 0 {
		t.Fatalf("текущая задача не пропущена: %+v", hits)
	}

	// /reset стирает и из поиска
	if err := st.resetHistory(1); err != nil {
		t.Fatal(err)
	}
	if hits, _ = st.recall(1, "задержки", recallOpts{}); len(hits) != 0 {
		t.Fatalf("после /reset реплики находятся: %+v", hits)
	}

	if _, err := st.recall(1, "да и в Go", recallOpts{}); err == nil {
		t.Fatal("запрос без слов от трёх букв должен вернуть ошибку")
	}
}

// База, жившая до появления индекса: реплики должны попасть в него при
// первом открытии.
func TestRecallBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.db")
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.addMessage(1, "user", "Старый разговор про кружки Кристины", 0)
	_ = st.addMessage(1, "user", "Стёртое про кружки", 0)
	_, _ = st.db.Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE content LIKE 'Стёртое%'`)
	// как будто индекса не было
	for _, q := range []string{`DROP TABLE recall_fts`, `DELETE FROM kv WHERE key = 'recall_fts'`} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	st.db.Close()

	st, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	hits, err := st.recall(1, "кружок", recallOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Excerpt, "Старый") {
		t.Fatalf("бэкфилл: %+v", hits)
	}
	// повторное открытие не дублирует строки
	st.db.Close()
	st, _ = openStore(path)
	if hits, _ = st.recall(1, "кружок", recallOpts{}); len(hits) != 1 {
		t.Fatalf("повторный бэкфилл задвоил: %+v", hits)
	}
}

func TestRecallTool(t *testing.T) {
	b := newBot(botConfig{historyTurns: 1}, nil, nil, nil, nil, testStore(t))
	_ = b.store.addMessage(7, "user", "Какой бакет для кэша образов?", 0)
	_ = b.store.addMessage(7, "assistant", "s3://lmify-models/kristina-cache", 0)
	_ = b.store.addMessage(7, "user", "А что с бакетом сейчас?", 0)
	_ = b.store.addMessage(7, "assistant", "Всё на месте.", 0)

	tl := b.toolByName("recall")
	if tl == nil {
		t.Fatal("recall не зарегистрирован")
	}
	args, _ := json.Marshal(map[string]any{"query": "бакет"})
	out, err := tl.run(context.Background(), 7, args)
	if err != nil {
		t.Fatal(err)
	}
	// окно истории — последние 2 реплики; находится только первая
	if !strings.Contains(out, "Какой бакет") || strings.Contains(out, "что с бакетом") || !strings.Contains(out, "владелец") {
		t.Fatalf("выдача recall: %q", out)
	}
}
