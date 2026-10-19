package web

import (
	"encoding/json"
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
	raw := state(s.app.store.DB, "budget_batch_pending")
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
	return s.savePending(remaining)
}

// Caller holds changeMu so a concurrent settings save cannot restore an older schedule.
func (s *Scheduler) finishBatch() error {
	tx, err := s.app.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	next := time.Now().Unix() + int64(s.app.Config().DefaultInterval)
	if _, err = tx.Exec("UPDATE monitors SET next_run=? WHERE enabled=1", next); err != nil {
		return err
	}
	for _, key := range []string{"budget_batch_pending", "budget_batch_failed"} {
		if _, err = tx.Exec(database.Upsert(tx.Dialect, "INSERT INTO web_state VALUES(?,?)", []string{"key"}, []string{"value"}), key, ""); err != nil {
			return err
		}
	}
	return tx.Commit()
}
