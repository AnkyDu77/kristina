package main

// Квитанции генераций Higgsfield: что заказано, кем (чат, задача), за
// сколько, где лежит результат. По ним — лимит месяца и пробуждение
// задач, ждущих генерацию (hf_wait).

import (
	"strings"
	"time"
)

type hfJob struct {
	ID       int64
	JobID    string // id задачи у Higgsfield
	ChatID   int64
	TaskID   int64 // 0 — заказано из чата
	Model    string
	Prompt   string
	Name     string // куда сохранить в workspace, без расширения
	Cost     float64
	Status   string // pending | done | failed
	HFStatus string // как назвал его Higgsfield (completed, nsfw, …)
	File     string // путь результата в workspace
	Error    string
	Created  time.Time
	Reported bool // результат уже отдан задаче через hf_wait
}

const hfJobCols = `id, job_id, chat_id, task_id, model, prompt, name, cost, status, hf_status, file, error, created_at, reported`

func scanHFJob(row interface{ Scan(...any) error }) (*hfJob, error) {
	var j hfJob
	var created int64
	var reported int
	if err := row.Scan(&j.ID, &j.JobID, &j.ChatID, &j.TaskID, &j.Model, &j.Prompt, &j.Name, &j.Cost,
		&j.Status, &j.HFStatus, &j.File, &j.Error, &created, &reported); err != nil {
		return nil, err
	}
	j.Created, j.Reported = time.Unix(created, 0), reported != 0
	return &j, nil
}

func (s *store) addHFJob(j *hfJob) (int64, error) {
	if j.Created.IsZero() {
		j.Created = time.Now()
	}
	res, err := s.db.Exec(`INSERT INTO hf_jobs(job_id, chat_id, task_id, model, prompt, name, cost, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, j.JobID, j.ChatID, j.TaskID, j.Model, j.Prompt, j.Name, j.Cost, j.Created.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *store) queryHFJobs(q string, args ...any) ([]*hfJob, error) {
	rows, err := s.db.Query(`SELECT `+hfJobCols+` FROM hf_jobs `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*hfJob
	for rows.Next() {
		j, err := scanHFJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *store) pendingHFJobs() ([]*hfJob, error) {
	return s.queryHFJobs(`WHERE status = 'pending' ORDER BY id`)
}

func (s *store) recentHFJobs(chatID int64, n int) ([]*hfJob, error) {
	return s.queryHFJobs(`WHERE chat_id = ? ORDER BY id DESC LIMIT ?`, chatID, n)
}

// unreportedHFJobs — готовые (или упавшие) задачи генерации, о которых
// задача ещё не знает.
func (s *store) unreportedHFJobs(taskID int64) ([]*hfJob, error) {
	return s.queryHFJobs(`WHERE task_id = ? AND status != 'pending' AND reported = 0 ORDER BY id`, taskID)
}

func (s *store) markHFReported(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	_, err := s.db.Exec(`UPDATE hf_jobs SET reported = 1 WHERE id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, args...)
	return err
}

// finishHFJob — итог генерации; только из pending (сторож мог упереться
// в рестарт посреди скачивания — второй раз итог не пишем).
func (s *store) finishHFJob(id int64, status, hfStatus, file, errText string) (bool, error) {
	res, err := s.db.Exec(`UPDATE hf_jobs SET status = ?, hf_status = ?, file = ?, error = ?, done_at = ?
		WHERE id = ? AND status = 'pending'`, status, hfStatus, file, errText, time.Now().Unix(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// hfSpent — потрачено с since: всё, кроме упавшего (его Higgsfield вернул).
func (s *store) hfSpent(since time.Time) float64 {
	var v float64
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(cost), 0) FROM hf_jobs WHERE status != 'failed' AND created_at >= ?`, since.Unix()).Scan(&v)
	return v
}

func (s *store) hfPendingCount() int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM hf_jobs WHERE status = 'pending'`).Scan(&n)
	return n
}

func (s *store) hfPendingForTask(taskID int64) int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM hf_jobs WHERE task_id = ? AND status = 'pending'`, taskID).Scan(&n)
	return n
}
