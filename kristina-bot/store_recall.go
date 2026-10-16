package main

// Поиск по прошлому (инструмент recall): старые реплики, которые уже не
// влезают в историю промпта, и фоновые задачи (цель + журнал передачи;
// итог задачи и так лежит в messages).
//
// Полнотекстовый индекс FTS5 — встроен в modernc, новой инфраструктуры не
// нужно. Токенайзер trigram ищет подстроки: русская морфология без
// стеммера ловится обрезкой окончаний у слов запроса (recallTerms).
//
// rowid индекса: id реплики для messages, −id для задач. Индекс держат в
// актуальном виде триггеры, а soft delete (/reset, удалённая задача)
// убирает строку из индекса: что стёрто из поля зрения модели, то и не
// вспоминается.

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

const recallSchema = `
CREATE VIRTUAL TABLE IF NOT EXISTS recall_fts USING fts5(
    body,
    kind UNINDEXED,       -- user | assistant | task
    chat_id UNINDEXED,
    created_at UNINDEXED,
    tokenize = 'trigram'
);

CREATE TRIGGER IF NOT EXISTS messages_recall_ins AFTER INSERT ON messages
WHEN new.deleted_at IS NULL BEGIN
    INSERT INTO recall_fts(rowid, body, kind, chat_id, created_at)
    VALUES (new.id, new.content, new.role, new.chat_id, new.created_at);
END;

CREATE TRIGGER IF NOT EXISTS messages_recall_del AFTER UPDATE OF deleted_at ON messages
WHEN new.deleted_at IS NOT NULL BEGIN
    DELETE FROM recall_fts WHERE rowid = new.id;
END;

CREATE TRIGGER IF NOT EXISTS tasks_recall_ins AFTER INSERT ON tasks BEGIN
    INSERT INTO recall_fts(rowid, body, kind, chat_id, created_at)
    VALUES (-new.id, new.goal, 'task', new.chat_id, new.created_at);
END;

-- Журнал пишется после каждого шага (saveTaskProgress) — переиндексация
-- строки на каждый шаг копеечная
CREATE TRIGGER IF NOT EXISTS tasks_recall_upd AFTER UPDATE OF goal, journal, deleted_at ON tasks BEGIN
    DELETE FROM recall_fts WHERE rowid = -new.id;
    INSERT INTO recall_fts(rowid, body, kind, chat_id, created_at)
    SELECT -new.id, new.goal || char(10) || char(10) || new.journal, 'task', new.chat_id, new.created_at
    WHERE new.deleted_at IS NULL;
END;
`

// initRecall — после migrations: триггер задач смотрит на tasks.journal,
// а она приезжает ALTER-ом. Строки, накопленные до появления индекса,
// заливаются в него один раз (метка в kv).
func (s *store) initRecall() error {
	if _, err := s.db.Exec(recallSchema); err != nil {
		return err
	}
	if s.getKV("recall_fts") != "" {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM recall_fts`,
		`INSERT INTO recall_fts(rowid, body, kind, chat_id, created_at)
			SELECT id, content, role, chat_id, created_at FROM messages WHERE deleted_at IS NULL`,
		`INSERT INTO recall_fts(rowid, body, kind, chat_id, created_at)
			SELECT -id, goal || char(10) || char(10) || journal, 'task', chat_id, created_at FROM tasks WHERE deleted_at IS NULL`,
		`INSERT INTO kv(key, value) VALUES('recall_fts', '1')
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type recallHit struct {
	ID      int64 // id реплики; у задачи — id задачи
	Kind    string
	At      time.Time
	Excerpt string
}

type recallOpts struct {
	limit int
	// skipFrom — реплики с id ≥ skipFrom модель и так видит в истории
	// промпта (0 — не пропускать)
	skipFrom int64
	// skipTask — текущая задача: её цель и журнал уже в ходе
	skipTask int64
}

// recallExcerpt — сколько текста вокруг совпадения отдаём модели. snippet()
// из FTS5 не годится: у trigram «токен» — три символа, и его потолок в 64
// токена — это пара слов.
const recallExcerpt = 700

// recall ищет по прошлому чата; самые релевантные (bm25) — первыми.
func (s *store) recall(chatID int64, query string, o recallOpts) ([]recallHit, error) {
	terms := recallTerms(query)
	if len(terms) == 0 {
		return nil, fmt.Errorf("в запросе нет слов от трёх букв — trigram-поиск по ним не работает")
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + t + `"`
	}
	if o.limit <= 0 {
		o.limit = 6
	}
	skipFrom := o.skipFrom
	if skipFrom <= 0 {
		skipFrom = 1 << 62
	}
	rows, err := s.db.Query(`SELECT rowid, kind, created_at, body FROM recall_fts
		WHERE recall_fts MATCH ? AND chat_id = ? AND rowid < ? AND rowid != ?
		ORDER BY rank LIMIT ?`,
		strings.Join(quoted, " OR "), chatID, skipFrom, -o.skipTask, o.limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recallHit
	for rows.Next() {
		var (
			h        recallHit
			at, body string
		)
		if err := rows.Scan(&h.ID, &h.Kind, &at, &body); err != nil {
			return nil, err
		}
		if h.ID < 0 {
			h.ID = -h.ID
		}
		h.At, _ = time.Parse(time.DateTime, at) // CURRENT_TIMESTAMP — UTC
		h.Excerpt = excerptAround(body, terms, recallExcerpt)
		out = append(out, h)
	}
	return out, rows.Err()
}

// visibleFrom — id самой старой реплики, которую модель видит в истории
// промпта (последние limit); 0 — истории нет.
func (s *store) visibleFrom(chatID int64, limit int) int64 {
	var id *int64
	_ = s.db.QueryRow(`SELECT MIN(id) FROM (SELECT id FROM messages
		WHERE chat_id = ? AND deleted_at IS NULL ORDER BY id DESC LIMIT ?)`, chatID, limit).Scan(&id)
	if id == nil {
		return 0
	}
	return *id
}

const maxRecallTerms = 12

// recallTerms — слова запроса для trigram-поиска: в нижнем регистре, не
// короче трёх символов (короче trigram не ищет), у кириллицы срезано
// окончание — «задержки» найдёт и «задержка», «памяти» — и «память».
// Грубо, но стеммер ради одного владельца тащить незачем: лишнее
// совпадение отсеет ранжирование, а пропуск не исправит ничто.
func recallTerms(query string) []string {
	words := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := map[string]bool{}
	var out []string
	for _, w := range words {
		r := []rune(w)
		if isCyrillic(r) {
			switch {
			case len(r) >= 6:
				r = r[:len(r)-2]
			case len(r) >= 4:
				r = r[:len(r)-1]
			}
		}
		if len(r) < 3 || seen[string(r)] {
			continue
		}
		seen[string(r)] = true
		out = append(out, string(r))
		if len(out) == maxRecallTerms {
			break
		}
	}
	return out
}

func isCyrillic(r []rune) bool {
	for _, c := range r {
		if !unicode.Is(unicode.Cyrillic, c) {
			return false
		}
	}
	return true
}

// excerptAround — кусок текста около первого совпадения с любым из
// терминов (если текст длиннее n рун).
func excerptAround(body string, terms []string, n int) string {
	r := []rune(body)
	if len(r) <= n {
		return body
	}
	// unicode.ToLower по руне длину не меняет — позиции в lower и r совпадают
	lower := make([]rune, len(r))
	for i, c := range r {
		lower[i] = unicode.ToLower(c)
	}
	at := -1
	for _, t := range terms {
		if i := runeIndex(lower, []rune(t)); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	start := max(0, at-n/3)
	end := min(len(r), start+n)
	start = max(0, end-n)
	out := string(r[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(r) {
		out += "…"
	}
	return out
}

func runeIndex(s, sub []rune) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := range sub {
			if s[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
