package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"xm/internal/database"
)

type CollectionJob struct {
	ID         int64  `json:"id"`
	Target     string `json:"target"`
	Source     string `json:"source"`
	Status     string `json:"status"`
	QueuedAt   int64  `json:"queued_at"`
	QueueOrder int64  `json:"-"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
	RunID      int64  `json:"run_id"`
	Attempts   int    `json:"attempts"`
	RetryAt    int64  `json:"retry_at,omitempty"`
	RetryCount int    `json:"retry_count,omitempty"`
	Message    string `json:"message"`
	Mode       string `json:"mode"`
	Position   int    `json:"position,omitempty"`
	PostsRead  int    `json:"posts_read,omitempty"`
	NewPosts   int    `json:"new_posts,omitempty"`
}

const (
	collectionModeIncremental = "incremental"
	collectionModeBackfill    = "backfill"
	collectionModeRescan      = "rescan"
)

func normalizeCollectionMode(mode string) (string, error) {
	if mode == "" {
		return collectionModeIncremental, nil
	}
	switch mode {
	case collectionModeIncremental, collectionModeBackfill, collectionModeRescan:
		return mode, nil
	default:
		return "", errors.New("不支持的采集模式")
	}
}

func automaticCollectionMode(m Monitor) string {
	if baselineReady(m.BaselineStatus) && (!m.Replies || baselineReady(m.RepliesBaseline)) {
		return collectionModeIncremental
	}
	return collectionModeBackfill
}

func baselineReady(status string) bool {
	return status == "visible_history_complete" || status == "partial"
}

func resetMonitorBaseline(tx *database.Tx, target string) error {
	if _, err := tx.Exec("DELETE FROM collection_progress WHERE target=?", target); err != nil {
		return err
	}
	_, err := tx.Exec("UPDATE monitors SET catchup=0,catchup_post_anchor='',catchup_reply_anchor='',catchup_scroll_budget=0,baseline_status='not_started',posts_baseline_status='not_started',replies_baseline_status=CASE WHEN replies=1 THEN 'not_started' ELSE 'not_enabled' END,baseline_started=0,baseline_finished=0,baseline_stop_reason='' WHERE username=?", target)
	return err
}

func ensureCollectionModeSchema(db *database.DB) error {
	for _, table := range []struct {
		name       string
		migrations []struct{ name, definition string }
	}{
		{"collection_jobs", []struct{ name, definition string }{
			{"mode", "VARCHAR(16) NOT NULL DEFAULT 'incremental'"},
			{"retry_at", "BIGINT NOT NULL DEFAULT 0"},
			{"retry_count", "INT NOT NULL DEFAULT 0"},
		}},
		{"monitors", []struct{ name, definition string }{
			{"baseline_status", "VARCHAR(32) NOT NULL DEFAULT 'not_started'"},
			{"posts_baseline_status", "VARCHAR(32) NOT NULL DEFAULT 'not_started'"},
			{"replies_baseline_status", "VARCHAR(32) NOT NULL DEFAULT 'not_enabled'"},
			{"baseline_started", "BIGINT NOT NULL DEFAULT 0"},
			{"baseline_finished", "BIGINT NOT NULL DEFAULT 0"},
			{"baseline_stop_reason", "VARCHAR(255) NOT NULL DEFAULT ''"},
		}},
	} {
		rows, err := db.Query("SELECT * FROM " + table.name + " WHERE 1=0")
		if err != nil {
			return err
		}
		columns, err := rows.Columns()
		rows.Close()
		if err != nil {
			return err
		}
		has := make(map[string]bool, len(columns))
		for _, column := range columns {
			has[column] = true
		}
		for _, migration := range table.migrations {
			if !has[migration.name] {
				if _, err = db.Exec("ALTER TABLE " + table.name + " ADD COLUMN " + migration.name + " " + migration.definition); err != nil {
					return err
				}
			}
		}
	}
	_, err := db.Exec("UPDATE monitors SET replies_baseline_status='not_started' WHERE replies=1 AND replies_baseline_status='not_enabled'")
	if err != nil {
		return err
	}
	// Repair the old scope-change state: replies were enabled without making
	// the overall baseline incomplete. Its incremental cursor cannot prove that
	// historical replies have been collected.
	condition := "replies=1 AND replies_baseline_status='not_started' AND baseline_status IN ('visible_history_complete','partial')"
	if _, err = db.Exec("DELETE FROM collection_progress WHERE target IN (SELECT username FROM monitors WHERE " + condition + ")"); err != nil {
		return err
	}
	if _, err = db.Exec("UPDATE monitors SET baseline_status='not_started',baseline_finished=0,baseline_stop_reason='',catchup=0,catchup_post_anchor='',catchup_reply_anchor='',catchup_scroll_budget=0 WHERE " + condition); err != nil {
		return err
	}
	_, err = db.Exec("UPDATE collection_jobs SET mode='backfill' WHERE finished_at=0 AND target IN (SELECT username FROM monitors WHERE baseline_status IN ('not_started','backfilling'))")
	return err
}

func ensureRunJobColumn(db *database.DB) error {
	rows, err := db.Query("SELECT * FROM runs WHERE 1=0")
	if err != nil {
		return err
	}
	columns, err := rows.Columns()
	rows.Close()
	if err != nil {
		return err
	}
	found := false
	for _, column := range columns {
		if column == "job_id" {
			found = true
			break
		}
	}
	if !found {
		if _, err = db.Exec("ALTER TABLE runs ADD COLUMN job_id BIGINT NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	var indexes int
	if db.Dialect == "mysql" {
		err = db.QueryRow("SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='runs' AND index_name='runs_job'").Scan(&indexes)
	} else {
		err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='runs_job'").Scan(&indexes)
	}
	if err != nil {
		return err
	}
	if indexes == 0 {
		_, err = db.Exec("CREATE INDEX runs_job ON runs(job_id)")
	}
	return err
}

func scanCollectionJob(row scanner) (CollectionJob, error) {
	var job CollectionJob
	err := row.Scan(&job.ID, &job.Target, &job.Source, &job.Status, &job.QueuedAt, &job.QueueOrder, &job.StartedAt, &job.FinishedAt, &job.RunID, &job.Attempts, &job.RetryAt, &job.RetryCount, &job.Message, &job.Mode)
	return job, err
}

const collectionJobColumns = "id,target,source,status,queued_at,queue_order,started_at,finished_at,run_id,attempts,retry_at,retry_count,message,mode"

// ensureCollectionJob turns legacy monitor flags and automatic batches into a
// durable FIFO entry. The scheduler holds changeMu while calling it.
func ensureCollectionJob(db *database.DB, target, source string, modes ...string) (CollectionJob, error) {
	requestedMode := collectionModeIncremental
	if len(modes) > 0 {
		requestedMode = modes[0]
	}
	mode, err := normalizeCollectionMode(requestedMode)
	if err != nil {
		return CollectionJob{}, err
	}
	job, err := scanCollectionJob(db.QueryRow("SELECT "+collectionJobColumns+" FROM collection_jobs WHERE target=? AND finished_at=0 ORDER BY id LIMIT 1", target))
	if err == nil {
		return job, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return CollectionJob{}, err
	}
	now := time.Now().Unix()
	order := time.Now().UnixNano()
	result, err := db.Exec("INSERT INTO collection_jobs(target,source,status,queued_at,queue_order,mode) VALUES(?,?,'queued',?,?,?)", target, source, now, order, mode)
	if err != nil {
		return CollectionJob{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return CollectionJob{}, err
	}
	return CollectionJob{ID: id, Target: target, Source: source, Status: "queued", QueuedAt: now, QueueOrder: order, Mode: mode}, nil
}

func enqueueCollectionJob(tx *database.Tx, target, source, mode, message string) (int64, error) {
	mode, err := normalizeCollectionMode(mode)
	if err != nil {
		return 0, err
	}
	var id int64
	var existingSource, status string
	err = tx.QueryRow("SELECT id,source,status FROM collection_jobs WHERE target=? AND finished_at=0 ORDER BY id LIMIT 1", target).Scan(&id, &existingSource, &status)
	if err == nil {
		if status == "running" {
			return 0, errors.New("账号已在队列或正在采集")
		}
		// A manual request consumes the target's existing automatic/retry slot;
		// it must not create duplicate work or bypass a global cooldown.
		_, err = tx.Exec("UPDATE collection_jobs SET source=?,mode=?,message=? WHERE id=?", source, mode, message, id)
		return id, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	now := time.Now()
	result, err := tx.Exec("INSERT INTO collection_jobs(target,source,status,queued_at,queue_order,message,mode) VALUES(?,?,'queued',?,?,?,?)", target, source, now.Unix(), now.UnixNano(), message, mode)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func markCollectionJobRunning(tx *database.Tx, id int64, started int64) error {
	_, err := tx.Exec("UPDATE collection_jobs SET status='running',started_at=CASE WHEN started_at=0 THEN ? ELSE started_at END,attempts=attempts+1,retry_at=0,message='' WHERE id=? AND finished_at=0", started, id)
	return err
}

func finishCollectionJob(db *database.DB, id, runID int64, status, message string, retry bool) error {
	if retry {
		now := time.Now()
		_, err := db.Exec("UPDATE collection_jobs SET status=?,queued_at=?,queue_order=?,run_id=?,retry_at=0,message=? WHERE id=? AND finished_at=0", status, now.Unix(), now.UnixNano(), runID, message, id)
		return err
	}
	if _, err := db.Exec("UPDATE collection_jobs SET status=?,finished_at=?,run_id=?,retry_at=0,retry_count=0,message=? WHERE id=? AND finished_at=0", status, time.Now().Unix(), runID, message, id); err != nil {
		return err
	}
	_, err := db.Exec("DELETE FROM collection_jobs WHERE finished_at>0 AND id < (SELECT COALESCE(MIN(id),0) FROM (SELECT id FROM collection_jobs WHERE finished_at>0 ORDER BY id DESC LIMIT 1000) recent)")
	return err
}

func retryTimelineCollectionJob(db *database.DB, id, runID int64, message string, retryAt int64, retryCount int) error {
	now := time.Now()
	_, err := db.Exec("UPDATE collection_jobs SET status='retrying',queued_at=?,queue_order=?,run_id=?,retry_at=?,retry_count=?,message=? WHERE id=? AND finished_at=0", now.Unix(), now.UnixNano(), runID, retryAt, retryCount, message, id)
	return err
}

func timelineRetryBackoff(previous int) (int, time.Duration) {
	count := min(previous+1, 3)
	return count, []time.Duration{5 * time.Second, 15 * time.Second, 45 * time.Second}[count-1]
}

// Stopping a monitor also closes its running slot. The worker may already have
// cleared its cancellation handle but not yet persisted a retry; that retry must
// not revive a task explicitly stopped by the user.
func cancelMonitorJobs(db *database.DB, target, message string) error {
	_, err := db.Exec("UPDATE collection_jobs SET status='cancelled',finished_at=?,message=? WHERE target=? AND finished_at=0", time.Now().Unix(), message, target)
	return err
}

var (
	errCollectionJobNotFound = errors.New("排队任务不存在")
	errCollectionJobRunning  = errors.New("任务已经开始执行")
)

// cancelWaitingCollectionJob cancels one queue entry without disabling its
// monitor. Removing it from the durable batch in the same transaction prevents
// the scheduler from recreating the cancelled slot during the current batch.
func cancelWaitingCollectionJob(db *database.DB, id int64) (string, error) {
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var target, status string
	var finished int64
	if err = tx.QueryRow("SELECT target,status,finished_at FROM collection_jobs WHERE id=?", id).Scan(&target, &status, &finished); errors.Is(err, sql.ErrNoRows) || finished != 0 {
		return "", errCollectionJobNotFound
	} else if err != nil {
		return "", err
	}
	if status == "running" {
		return "", errCollectionJobRunning
	}
	now := time.Now().Unix()
	result, err := tx.Exec("UPDATE collection_jobs SET status='cancelled',finished_at=?,message='任务已由用户取消' WHERE id=? AND finished_at=0 AND status<>'running'", now, id)
	if err != nil {
		return "", err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return "", errCollectionJobRunning
	}
	if _, err = tx.Exec("UPDATE monitors SET requested=0 WHERE username=?", target); err != nil {
		return "", err
	}
	var raw string
	err = tx.QueryRow("SELECT value FROM web_state WHERE `key`='budget_batch_pending'").Scan(&raw)
	if err == nil && raw != "" {
		var pending []string
		if err = json.Unmarshal([]byte(raw), &pending); err != nil {
			return "", err
		}
		remaining := make([]string, 0, len(pending))
		for _, name := range pending {
			if name != target {
				remaining = append(remaining, name)
			}
		}
		encoded, marshalErr := json.Marshal(remaining)
		if marshalErr != nil {
			return "", marshalErr
		}
		if _, err = tx.Exec(database.Upsert(tx.Dialect, "INSERT INTO web_state VALUES(?,?)", []string{"key"}, []string{"value"}), "budget_batch_pending", string(encoded)); err != nil {
			return "", err
		}
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return target, tx.Commit()
}

func activityJobs(db *database.DB) (*CollectionJob, []CollectionJob, error) {
	rows, err := db.Query("SELECT " + collectionJobColumns + " FROM collection_jobs WHERE finished_at=0 ORDER BY CASE WHEN status='running' THEN 0 ELSE 1 END,queue_order,id")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	queue := []CollectionJob{}
	var current *CollectionJob
	position := 0
	for rows.Next() {
		job, scanErr := scanCollectionJob(rows)
		if scanErr != nil {
			return nil, nil, scanErr
		}
		if job.Status == "running" && current == nil {
			copy := job
			current = &copy
			continue
		}
		position++
		job.Position = position
		queue = append(queue, job)
	}
	if current != nil && current.RunID > 0 {
		_ = db.QueryRow("SELECT posts_read,new_posts FROM run_metrics WHERE run_id=?", current.RunID).Scan(&current.PostsRead, &current.NewPosts)
	}
	return current, queue, rows.Err()
}
