package main

// Запросы к задачам и мониторам.
//
// Статус задачи меняется только условными переходами (WHERE status IN …):
// раннер, кнопки и ответы владельца работают параллельно, и «пауза»,
// нажатая посреди шага, не должна затереться сохранением этого шага.
// Поэтому saveTaskProgress статус не пишет вовсе.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

type planStep struct {
	Title  string `json:"title"`
	Status string `json:"status"` // todo | doing | done | skipped
}

// pendingCall — припаркованный ход агента: первый вызов ждёт владельца,
// остальные — из того же хода модели, ещё не выполненные.
type pendingCall struct {
	Calls    []toolCall `json:"calls"`
	Kind     string     `json:"kind"` // input | approval
	ActionID int64      `json:"action_id,omitempty"`
	Summary  string     `json:"summary,omitempty"`
	Question string     `json:"question,omitempty"`
	Options  []string   `json:"options,omitempty"`
	// Ответ владельца: текст (input) или approved | rejected (approval)
	Answer   string `json:"answer,omitempty"`
	Decision string `json:"decision,omitempty"`
}

func (p *pendingCall) resolved() bool {
	return p.Answer != "" || p.Decision != ""
}

type task struct {
	ID, ChatID int64
	Goal       string
	Status     string
	Plan       []planStep
	Transcript []chatMsg
	Steps      int
	Note       string
	Pending    *pendingCall
	Result     string
	Error      string
	CardMsgID  int64
	AskMsgID   int64
}

const taskCols = `id, chat_id, goal, status, plan, transcript, steps, note, pending, result, error, card_msg_id, ask_msg_id`

func scanTask(row interface{ Scan(...any) error }) (*task, error) {
	var t task
	var plan, transcript, pending string
	if err := row.Scan(&t.ID, &t.ChatID, &t.Goal, &t.Status, &plan, &transcript, &t.Steps, &t.Note,
		&pending, &t.Result, &t.Error, &t.CardMsgID, &t.AskMsgID); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(plan), &t.Plan)
	_ = json.Unmarshal([]byte(transcript), &t.Transcript)
	if pending != "" {
		t.Pending = &pendingCall{}
		if json.Unmarshal([]byte(pending), t.Pending) != nil {
			t.Pending = nil
		}
	}
	return &t, nil
}

func jsonStr(v any) string {
	if v == nil {
		return ""
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

func (s *store) createTask(chatID int64, goal string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO tasks(chat_id, goal) VALUES(?, ?)`, chatID, goal)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *store) getTask(id int64) (*task, error) {
	return scanTask(s.db.QueryRow(`SELECT `+taskCols+` FROM tasks WHERE id = ? AND deleted_at IS NULL`, id))
}

func (s *store) taskStatus(id int64) string {
	var st string
	_ = s.db.QueryRow(`SELECT status FROM tasks WHERE id = ?`, id).Scan(&st)
	return st
}

// listTasks — последние задачи чата: незавершённые первыми.
func (s *store) listTasks(chatID int64, limit int) ([]*task, error) {
	rows, err := s.db.Query(`SELECT `+taskCols+` FROM tasks WHERE chat_id = ? AND deleted_at IS NULL
		ORDER BY status IN ('done', 'failed', 'cancelled'), id DESC LIMIT ?`, chatID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// claimTask забирает старейшую задачу из очереди в работу.
func (s *store) claimTask() (int64, bool, error) {
	var id int64
	err := s.db.QueryRow(`UPDATE tasks SET status = 'running', updated_at = CURRENT_TIMESTAMP
		WHERE id = (SELECT id FROM tasks WHERE status = 'queued' AND deleted_at IS NULL ORDER BY id LIMIT 1)
		RETURNING id`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// setTaskStatus — переход из одного из from в to; false — задача уже в
// другом состоянии (кнопку нажали поздно или дважды).
func (s *store) setTaskStatus(id int64, to string, from ...string) (bool, error) {
	args := []any{to, id}
	for _, f := range from {
		args = append(args, f)
	}
	res, err := s.db.Exec(`UPDATE tasks SET status = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND deleted_at IS NULL AND status IN (?`+strings.Repeat(",?", len(from)-1)+`)`, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// saveTaskProgress — всё, кроме статуса.
func (s *store) saveTaskProgress(t *task) error {
	_, err := s.db.Exec(`UPDATE tasks SET plan = ?, transcript = ?, steps = ?, note = ?, pending = ?,
		card_msg_id = ?, ask_msg_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		jsonStr(t.Plan), jsonStr(t.Transcript), t.Steps, t.Note, pendingStr(t.Pending), t.CardMsgID, t.AskMsgID, t.ID)
	return err
}

func pendingStr(p *pendingCall) string {
	if p == nil {
		return ""
	}
	return jsonStr(p)
}

// finishTask — итог задачи; только из running: отменённую посреди шага
// задачу результат последнего шага не воскрешает.
func (s *store) finishTask(id int64, status, result, errText string) (bool, error) {
	res, err := s.db.Exec(`UPDATE tasks SET status = ?, result = ?, error = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = 'running'`, status, result, errText, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// answerTask записывает ответ владельца в припаркованный вызов. Ждавшая
// задача встаёт в очередь; поставленная на паузу ответ запоминает и
// продолжит, когда её снимут с паузы.
func (s *store) answerTask(id int64, p *pendingCall) (bool, error) {
	res, err := s.db.Exec(`UPDATE tasks SET pending = ?,
		status = CASE WHEN status = 'waiting' THEN 'queued' ELSE status END, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status IN ('waiting', 'paused')`, pendingStr(p), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *store) taskByAskMsg(chatID, msgID int64) (*task, error) {
	t, err := scanTask(s.db.QueryRow(`SELECT `+taskCols+` FROM tasks
		WHERE chat_id = ? AND ask_msg_id = ? AND deleted_at IS NULL`, chatID, msgID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// requeueRunning — на старте: задачи, бывшие в работе у прошлого
// процесса, продолжаются с последнего сохранённого шага.
func (s *store) requeueRunning() (int64, error) {
	res, err := s.db.Exec(`UPDATE tasks SET status = 'queued' WHERE status = 'running'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *store) countQueued() int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE status = 'queued' AND deleted_at IS NULL`).Scan(&n)
	return n
}

// ── Мониторы ───────────────────────────────────────────────────────────────

type monitor struct {
	ID, ChatID int64
	URL        string
	Kind       string
	Pattern    string
	Interval   time.Duration
	Status     string
	State      string
	LastText   string
	LastAlert  string
	Fails      int
	LastError  string
	NextRun    time.Time
}

const monitorCols = `id, chat_id, url, kind, pattern, interval_s, status, state, last_text, last_alert, fails, last_error, next_run_at`

func scanMonitor(row interface{ Scan(...any) error }) (*monitor, error) {
	var m monitor
	var interval, next int64
	if err := row.Scan(&m.ID, &m.ChatID, &m.URL, &m.Kind, &m.Pattern, &interval, &m.Status, &m.State,
		&m.LastText, &m.LastAlert, &m.Fails, &m.LastError, &next); err != nil {
		return nil, err
	}
	m.Interval = time.Duration(interval) * time.Second
	m.NextRun = time.Unix(next, 0)
	return &m, nil
}

func (s *store) createMonitor(m *monitor) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO monitors(chat_id, url, kind, pattern, interval_s, next_run_at)
		VALUES(?, ?, ?, ?, ?, ?)`, m.ChatID, m.URL, m.Kind, m.Pattern, int64(m.Interval/time.Second), m.NextRun.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *store) saveMonitor(m *monitor) error {
	_, err := s.db.Exec(`UPDATE monitors SET state = ?, last_text = ?, last_alert = ?, fails = ?, last_error = ?,
		next_run_at = ? WHERE id = ?`, m.State, m.LastText, m.LastAlert, m.Fails, m.LastError, m.NextRun.Unix(), m.ID)
	return err
}

func (s *store) getMonitor(chatID, id int64) (*monitor, error) {
	m, err := scanMonitor(s.db.QueryRow(`SELECT `+monitorCols+` FROM monitors
		WHERE chat_id = ? AND id = ? AND deleted_at IS NULL`, chatID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

func (s *store) listMonitors(chatID int64) ([]*monitor, error) {
	return s.queryMonitors(`SELECT `+monitorCols+` FROM monitors WHERE chat_id = ? AND deleted_at IS NULL ORDER BY id`, chatID)
}

func (s *store) dueMonitors(now time.Time) ([]*monitor, error) {
	return s.queryMonitors(`SELECT `+monitorCols+` FROM monitors
		WHERE status = 'active' AND deleted_at IS NULL AND next_run_at <= ? ORDER BY next_run_at`, now.Unix())
}

func (s *store) queryMonitors(q string, args ...any) ([]*monitor, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*monitor
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// setMonitorStatus — active | paused | deleted (soft delete).
func (s *store) setMonitorStatus(chatID, id int64, status string) (bool, error) {
	q := `UPDATE monitors SET status = ?, next_run_at = ? WHERE chat_id = ? AND id = ? AND deleted_at IS NULL`
	args := []any{status, time.Now().Unix(), chatID, id}
	if status == "deleted" {
		q = `UPDATE monitors SET deleted_at = CURRENT_TIMESTAMP WHERE chat_id = ? AND id = ? AND deleted_at IS NULL`
		args = []any{chatID, id}
	}
	res, err := s.db.Exec(q, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
