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
func rateLimitDeadline(now int64, interval, previousFailures int, err error, modes ...string) (int64, int) {
	mode := "balanced"
	if len(modes) > 0 {
		mode = modes[0]
	}
	minimum, maximum, margin := int64(15*60), int64(6*60*60), 5*time.Second
	switch mode {
	case "conservative":
		minimum, maximum, margin = 30*60, 12*60*60, 10*time.Second
	case "fast":
		minimum, maximum, margin = 5*60, 2*60*60, 3*time.Second
	}
	failures := min(max(previousFailures, 0), 15) + 1
	delay := int64(max(interval, int(minimum)))
	ceiling := max(delay, maximum)
	for n := 1; n < failures && delay < ceiling; n++ {
		delay = min(delay*2, ceiling)
	}
	until := now + delay
	var failure *archive.Failure
	if errors.As(err, &failure) && !failure.RetryAt.IsZero() {
		// Leave a small margin around upstream reset times.
		until = max(until, failure.RetryAt.Add(margin).Unix())
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
func upgradeRateLimitState(db *database.DB, interval int, mode string, now int64) error {
	gates, err := loadSchedulerState(db)
	if err != nil {
		return err
	}
	raw, err := readState(db, "rate_limit_failures")
	if err != nil {
		return err
	}
	if raw != "" {
		return nil
	}
	oldUntil := gates.CooldownUntil
	if oldUntil <= now {
		return nil
	}
	until, failures := rateLimitDeadline(oldUntil-900, interval, 0, nil, mode)
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
