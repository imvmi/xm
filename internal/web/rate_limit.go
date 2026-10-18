package web

import (
	"errors"
	"log"
	"strconv"
	"time"

	"xm/internal/archive"
	"xm/internal/database"
)

// Keep the counter across batches and restarts: a successful target before a
// rate-limited target must not return every retry to the shortest delay.
func rateLimitDeadline(now int64, interval, previousFailures int, err error) (int64, int) {
	failures := min(max(previousFailures, 0), 15) + 1
	delay := int64(max(interval, 15*60))
	ceiling := max(delay, int64(6*60*60))
	for n := 1; n < failures && delay < ceiling; n++ {
		delay = min(delay*2, ceiling)
	}
	until := now + delay
	var failure *archive.Failure
	if errors.As(err, &failure) && !failure.RetryAt.IsZero() {
		// Leave a small margin around upstream reset times.
		until = max(until, failure.RetryAt.Add(5*time.Second).Unix())
	}
	return until, failures
}

func saveRateLimit(tx *database.Tx, until int64, failures int) error {
	for _, entry := range []struct{ key, value string }{
		{"cooldown_until", strconv.FormatInt(until, 10)},
		{"rate_limit_failures", strconv.Itoa(failures)},
	} {
		if _, err := tx.Exec(database.Upsert(tx.Dialect, "INSERT INTO web_state VALUES(?,?)", []string{"key"}, []string{"value"}), entry.key, entry.value); err != nil {
			return err
		}
	}
	return nil
}

// Carry an active cooldown from the old fixed-15-minute policy into the new
// policy on upgrade. Never shorten it, or revive a stale, expired cooldown.
func upgradeRateLimitState(db *database.DB, interval int, now int64) error {
	if state(db, "rate_limit_failures") != "" {
		return nil
	}
	oldUntil, _ := strconv.ParseInt(state(db, "cooldown_until"), 10, 64)
	if oldUntil <= now {
		return nil
	}
	until, failures := rateLimitDeadline(oldUntil-900, interval, 0, nil)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = saveRateLimit(tx, max(oldUntil, until), failures); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	log.Printf("rate limit policy upgraded failures=%d retry_at=%s", failures, time.Unix(max(oldUntil, until), 0).UTC().Format(time.RFC3339))
	return nil
}
