package main

// sqlite-хранилище Кристины (modernc.org/sqlite — чистый Go, как в
// lmify-ui): история диалога, память о владельце, квитанции
// подтверждений, журнал инструментов и «почтовый ящик» вопросов,
// дождавшихся подъёма видеокарты.
//
// Во всей схеме soft delete: строки не удаляются, а помечаются
// deleted_at. /reset и «забудь» стирают из поля зрения модели, не из базы.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type store struct{ db *sql.DB }

func openStore(path string) (*store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// sqlite — однописательная БД; без лимита соединений конкурентные
	// записи ловят SQLITE_BUSY даже с WAL (грабля lmify)
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &store{db: db}, nil
}

const schema = `
-- Диалог: только реплики владельца и итоговые ответы. Ход агента
-- (вызовы инструментов и их выдача) в историю не попадает — он съел бы
-- контекст; он лежит в tool_runs.
CREATE TABLE IF NOT EXISTS messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id    INTEGER NOT NULL,
    role       TEXT NOT NULL,              -- user | assistant
    content    TEXT NOT NULL,
    steps      INTEGER NOT NULL DEFAULT 0, -- сколько инструментов понадобилось ответу
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS messages_chat ON messages(chat_id, id) WHERE deleted_at IS NULL;

-- Что Кристина знает о владельце. Попадает в системный промпт целиком.
CREATE TABLE IF NOT EXISTS memories (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id    INTEGER NOT NULL,
    text       TEXT NOT NULL,
    source     TEXT NOT NULL,              -- user (/remember) | agent (с подтверждением)
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS memories_chat ON memories(chat_id) WHERE deleted_at IS NULL;

-- Квитанции подтверждений: что агент просил сделать, что ответил
-- владелец и чем кончилось.
CREATE TABLE IF NOT EXISTS actions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id    INTEGER NOT NULL,
    tool       TEXT NOT NULL,
    args       TEXT NOT NULL,
    summary    TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending', -- pending | approved | rejected | expired
    result     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    decided_at TIMESTAMP,
    deleted_at TIMESTAMP
);

-- Журнал инструментов — для разбора «почему она так ответила».
CREATE TABLE IF NOT EXISTS tool_runs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id    INTEGER NOT NULL,
    tool       TEXT NOT NULL,
    args       TEXT NOT NULL,
    result     TEXT NOT NULL,
    error      TEXT NOT NULL DEFAULT '',
    ms         INTEGER NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP
);

-- Вопросы, пришедшие, пока видеокарта спала. Ответ на них — одним
-- заходом после подъёма: один прогон модели дешевле нескольких.
CREATE TABLE IF NOT EXISTS inbox (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id    INTEGER NOT NULL,
    from_id    INTEGER NOT NULL,
    text       TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    done_at    TIMESTAMP,
    deleted_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS inbox_pending ON inbox(chat_id) WHERE done_at IS NULL AND deleted_at IS NULL;

-- Настройки чатов и прочие мелочи (кружки вкл/выкл, id сессии поиска).
CREATE TABLE IF NOT EXISTS kv (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// ── История ────────────────────────────────────────────────────────────────

func (s *store) addMessage(chatID int64, role, content string, steps int) error {
	_, err := s.db.Exec(`INSERT INTO messages(chat_id, role, content, steps) VALUES(?, ?, ?, ?)`,
		chatID, role, content, steps)
	return err
}

// history — последние limit реплик по порядку, старые первыми.
func (s *store) history(chatID int64, limit int) ([]chatMsg, error) {
	rows, err := s.db.Query(`SELECT role, content FROM messages
		WHERE chat_id = ? AND deleted_at IS NULL ORDER BY id DESC LIMIT ?`, chatID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chatMsg
	for rows.Next() {
		var m chatMsg
		if err := rows.Scan(&m.Role, &m.Content); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	// Срез по LIMIT мог начаться с ответа без вопроса — модели это
	// непонятно, и шаблон чата Qwen ждёт user первым
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	for len(out) > 0 && out[0].Role != "user" {
		out = out[1:]
	}
	return out, rows.Err()
}

func (s *store) lastAnswer(chatID int64) (string, error) {
	var text string
	err := s.db.QueryRow(`SELECT content FROM messages
		WHERE chat_id = ? AND role = 'assistant' AND deleted_at IS NULL ORDER BY id DESC LIMIT 1`, chatID).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return text, err
}

func (s *store) resetHistory(chatID int64) error {
	_, err := s.db.Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP
		WHERE chat_id = ? AND deleted_at IS NULL`, chatID)
	return err
}

func (s *store) countMessages(chatID int64) int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = ? AND deleted_at IS NULL`, chatID).Scan(&n)
	return n
}

// ── Память ─────────────────────────────────────────────────────────────────

type memory struct {
	ID   int64
	Text string
}

func (s *store) addMemory(chatID int64, text, source string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO memories(chat_id, text, source) VALUES(?, ?, ?)`, chatID, text, source)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *store) memories(chatID int64) ([]memory, error) {
	rows, err := s.db.Query(`SELECT id, text FROM memories
		WHERE chat_id = ? AND deleted_at IS NULL ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []memory
	for rows.Next() {
		var m memory
		if err := rows.Scan(&m.ID, &m.Text); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// memory — одна живая запись; ok=false, если её нет или уже забыта.
func (s *store) memory(chatID, id int64) (m memory, ok bool, err error) {
	err = s.db.QueryRow(`SELECT id, text FROM memories
		WHERE chat_id = ? AND id = ? AND deleted_at IS NULL`, chatID, id).Scan(&m.ID, &m.Text)
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	return m, err == nil, err
}

func (s *store) forgetMemory(chatID, id int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE memories SET deleted_at = CURRENT_TIMESTAMP
		WHERE chat_id = ? AND id = ? AND deleted_at IS NULL`, chatID, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ── Подтверждения ──────────────────────────────────────────────────────────

func (s *store) createAction(chatID int64, tool, args, summary string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO actions(chat_id, tool, args, summary) VALUES(?, ?, ?, ?)`,
		chatID, tool, args, summary)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// decideAction переводит подтверждение из pending в status. Решение
// бывает ровно одно: кнопку нажали дважды, или она гонится с таймаутом —
// выигрывает первый, остальные получают false.
func (s *store) decideAction(chatID, id int64, status string) (bool, error) {
	res, err := s.db.Exec(`UPDATE actions SET status = ?, decided_at = CURRENT_TIMESTAMP
		WHERE chat_id = ? AND id = ? AND status = 'pending' AND deleted_at IS NULL`, status, chatID, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *store) finishAction(id int64, result string) error {
	_, err := s.db.Exec(`UPDATE actions SET result = ? WHERE id = ?`, result, id)
	return err
}

// expirePending — на старте: агент, ждавший ответа, умер вместе с
// прошлым процессом, и нажатие на старую кнопку ничего бы не сделало.
func (s *store) expirePending() (int64, error) {
	res, err := s.db.Exec(`UPDATE actions SET status = 'expired', decided_at = CURRENT_TIMESTAMP
		WHERE status = 'pending'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ── Журнал инструментов ────────────────────────────────────────────────────

func (s *store) logTool(chatID int64, tool, args, result, errText string, took time.Duration) error {
	_, err := s.db.Exec(`INSERT INTO tool_runs(chat_id, tool, args, result, error, ms) VALUES(?, ?, ?, ?, ?, ?)`,
		chatID, tool, args, result, errText, took.Milliseconds())
	return err
}

// ── Отложенные вопросы ─────────────────────────────────────────────────────

type inboxItem struct {
	ID   int64
	Text string
}

// addInbox откладывает вопрос до подъёма карты; возвращает, сколько их
// теперь ждёт в этом чате.
func (s *store) addInbox(chatID, fromID int64, text string) (int, error) {
	if _, err := s.db.Exec(`INSERT INTO inbox(chat_id, from_id, text) VALUES(?, ?, ?)`, chatID, fromID, text); err != nil {
		return 0, err
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM inbox
		WHERE chat_id = ? AND done_at IS NULL AND deleted_at IS NULL`, chatID).Scan(&n)
	return n, err
}

func (s *store) pendingInbox(chatID int64) ([]inboxItem, error) {
	rows, err := s.db.Query(`SELECT id, text FROM inbox
		WHERE chat_id = ? AND done_at IS NULL AND deleted_at IS NULL ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []inboxItem
	for rows.Next() {
		var it inboxItem
		if err := rows.Scan(&it.ID, &it.Text); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *store) doneInbox(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	_, err := s.db.Exec(`UPDATE inbox SET done_at = CURRENT_TIMESTAMP WHERE id IN (?`+
		strings.Repeat(",?", len(ids)-1)+`)`, args...)
	return err
}

func (s *store) inboxChats() ([]int64, error) {
	rows, err := s.db.Query(`SELECT DISTINCT chat_id FROM inbox WHERE done_at IS NULL AND deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *store) countInbox(chatID int64) int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM inbox
		WHERE chat_id = ? AND done_at IS NULL AND deleted_at IS NULL`, chatID).Scan(&n)
	return n
}

// ── kv ─────────────────────────────────────────────────────────────────────

func (s *store) getKV(key string) string {
	var v string
	_ = s.db.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&v)
	return v
}

func (s *store) setKV(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO kv(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
