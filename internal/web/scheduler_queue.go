package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"xm/internal/archive"
	"xm/internal/database"
)

type interruptedAttempt struct {
	target       string
	jobID, runID int64
	stateError   *completionStateError
}

// A state/database failure after collection must not strand running=1 forever.
// Retry this transaction at the next idle boundary; a process restart uses the
// normal startup recovery. Keep archive/checkpoints and existing cooldowns;
// reapply known upstream stop conditions before allowing any new work.
func (s *Scheduler) recoverAttempt(attempt interruptedAttempt) error {
	gates, err := loadSchedulerState(s.app.store.DB)
	if err != nil {
		return err
	}
	tx, err := s.app.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if failure := attempt.stateError; failure != nil && failure.collectionErr != nil {
		switch archive.FailureKind(failure.collectionErr) {
		case "rate":
			cfg := s.app.Config()
			until, count := rateLimitDeadline(time.Now().Unix(), cfg.DefaultInterval, gates.RateFailures, failure.collectionErr, cfg.RateLimitMode)
			if err = saveRateLimit(tx, max(until, gates.CooldownUntil), count); err != nil {
				return err
			}
		case "auth":
			if gates.CookieGeneration == failure.cookieGeneration {
				if _, err = tx.Exec(database.Upsert(tx.Dialect, "INSERT INTO web_state VALUES(?,?)", []string{"key"}, []string{"value"}), "cookie_blocked", failure.message); err != nil {
					return err
				}
			}
		case "access", "identity", "checkpoint":
			if _, err = tx.Exec("UPDATE monitors SET enabled=0,requested=0 WHERE username=?", attempt.target); err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE collection_jobs SET status='failed',finished_at=?,message=? WHERE id=? AND finished_at=0", time.Now().Unix(), failure.message, attempt.jobID); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(database.Upsert(tx.Dialect, "INSERT INTO web_state VALUES(?,?)", []string{"key"}, []string{"value"}), "budget_batch_failed", "1"); err != nil {
			return err
		}
	}
	message := "采集结果保存中断，已归档内容保留，等待重新执行"
	if _, err = tx.Exec("UPDATE monitors SET running=0,last_error=? WHERE username=? AND running=1", message, attempt.target); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE runs SET status='interrupted',finished_at=?,message=? WHERE id=? AND status='running'", time.Now().UTC().Format(time.RFC3339Nano), message, attempt.runID); err != nil {
		return err
	}
	now := time.Now()
	if _, err = tx.Exec("UPDATE collection_jobs SET status='queued',queued_at=?,queue_order=?,message=? WHERE id=? AND status='running' AND finished_at=0", now.Unix(), now.UnixNano(), message, attempt.jobID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Scheduler) savePending(targets []string) error {
	if targets == nil {
		targets = []string{}
	}
	raw, err := json.Marshal(targets)
	if err != nil {
		return err
	}
	return setState(s.app.store.DB, "budget_batch_pending", string(raw))
}

// Update the current durable list, never the snapshot taken before collection.
// Cancellation handlers hold the same lock and may have removed other targets
// while this task ran. A removed target must not be reintroduced by a retry.
func (s *Scheduler) updatePending(target string, retry bool) error {
	s.app.changeMu.Lock()
	defer s.app.changeMu.Unlock()
	tx, err := s.app.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRow("SELECT value FROM web_state WHERE `key`='budget_batch_pending'").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if raw == "" {
		return nil
	}
	var pending []string
	if err := json.Unmarshal([]byte(raw), &pending); err != nil {
		return err
	}
	remaining := make([]string, 0, len(pending))
	found := false
	for _, name := range pending {
		if name != target {
			remaining = append(remaining, name)
		} else {
			found = true
		}
	}
	if retry && found {
		remaining = append(remaining, target)
	}
	// Advance only this completed account's due schedule. Commit it with
	// removal from the durable batch so restart cannot lose the next run.
	if found && !retry {
		if err = scheduleNextMonitorRun(tx, target, time.Now().Unix()); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(remaining)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(database.Upsert(tx.Dialect, "INSERT INTO web_state VALUES(?,?)", []string{"key"}, []string{"value"}), "budget_batch_pending", string(encoded)); err != nil {
		return err
	}
	return tx.Commit()
}

// Caller holds changeMu. Schedules advance per account, never for new arrivals
// or other accounts outside this completed batch.
func (s *Scheduler) finishBatch() error {
	tx, err := s.app.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, key := range []string{"budget_batch_pending", "budget_batch_failed"} {
		if _, err = tx.Exec(database.Upsert(tx.Dialect, "INSERT INTO web_state VALUES(?,?)", []string{"key"}, []string{"value"}), key, ""); err != nil {
			return err
		}
	}
	return tx.Commit()
}
