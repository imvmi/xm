package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"xm/internal/database"
)

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
