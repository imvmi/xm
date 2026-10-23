package web

import (
	"encoding/json"
	"errors"
	"strconv"

	"xm/internal/database"
)

// Read all scheduling gates from one statement snapshot. Missing legacy keys
// have defaults; database errors and malformed persisted gates fail closed.
type schedulerState struct {
	CookieBlocked, CookieGeneration, Pending string
	CookieAttempted, BatchFailed             bool
	CooldownUntil                            int64
	RateFailures                             int
}

// Preserve a known upstream stop condition if the state needed to commit it
// cannot be read. Recovery must apply it before admitting another collection.
type completionStateError struct {
	err                       error
	collectionErr             error
	cookieGeneration, message string
}

func (e *completionStateError) Error() string { return e.err.Error() }
func (e *completionStateError) Unwrap() error { return e.err }

func loadSchedulerState(db *database.DB) (schedulerState, error) {
	var s schedulerState
	rows, err := db.Query("SELECT `key`,value FROM web_state WHERE `key` IN ('cookie_blocked','cookie_imported','cookie_api_attempted','budget_batch_pending','budget_batch_failed','cooldown_until','rate_limit_failures')")
	if err != nil {
		return s, errors.New("读取采集状态失败，已停止本轮调度")
	}
	defer rows.Close()
	values := map[string]string{}
	for rows.Next() {
		var key, value string
		if err = rows.Scan(&key, &value); err != nil {
			return s, errors.New("读取采集状态失败，已停止本轮调度")
		}
		values[key] = value
	}
	if rows.Err() != nil {
		return s, errors.New("读取采集状态失败，已停止本轮调度")
	}
	invalid := errors.New("采集状态损坏，已停止调度，请检查数据库或恢复备份")
	for _, key := range []string{"cookie_api_attempted", "budget_batch_failed"} {
		if v := values[key]; v != "" && v != "0" && v != "1" {
			return s, invalid
		}
	}
	if v := values["cooldown_until"]; v != "" {
		s.CooldownUntil, err = strconv.ParseInt(v, 10, 64)
		if err != nil || s.CooldownUntil < 0 {
			return s, invalid
		}
	}
	if v := values["rate_limit_failures"]; v != "" {
		s.RateFailures, err = strconv.Atoi(v)
		if err != nil || s.RateFailures < 0 {
			return s, invalid
		}
	}
	s.CookieBlocked, s.CookieGeneration, s.Pending = values["cookie_blocked"], values["cookie_imported"], values["budget_batch_pending"]
	s.CookieAttempted, s.BatchFailed = values["cookie_api_attempted"] == "1", values["budget_batch_failed"] == "1"
	if s.Pending != "" {
		var pending []string
		if json.Unmarshal([]byte(s.Pending), &pending) != nil {
			return s, invalid
		}
	}
	return s, nil
}
