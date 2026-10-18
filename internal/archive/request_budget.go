package archive

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/network"
	"xm/internal/database"
)

// These are conservative local policies, not claimed X limits. The rolling
// window covers all collection endpoints and all targets using the same login.
const requestBudgetWindow = 15 * time.Minute
const requestBudgetLimit = 30
const requestBudgetSpacing = 10 * time.Second

type endpointBudget struct {
	Limit     int   `json:"limit"`
	Remaining *int  `json:"remaining,omitempty"`
	Reset     int64 `json:"reset"`
	Status    int64 `json:"status"`
	Observed  int64 `json:"observed"`
	Responses int64 `json:"responses"`
}
type requestBudgetState struct {
	Requests   []int64                   `json:"requests"`
	Endpoints  map[string]endpointBudget `json:"endpoints"`
	PauseUntil int64                     `json:"pause_until"`
}
type RequestBudget struct {
	pending atomic.Int64
	mu      sync.Mutex
	state   requestBudgetState
	persist func(requestBudgetState) error
	now     func() time.Time
	spacing time.Duration
}

func (s *Store) LoadRequestBudget(cookies Cookies) (*RequestBudget, error) {
	digest := sha256.Sum256([]byte(cookies.AuthToken))
	scope := hex.EncodeToString(digest[:])
	b := &RequestBudget{now: time.Now, spacing: requestBudgetSpacing}
	var raw string
	err := s.DB.QueryRow("SELECT state FROM request_budgets WHERE scope=?", scope).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal([]byte(raw), &b.state); err != nil {
			return nil, errors.New("共享请求预算损坏，已停止采集")
		}
	}
	if b.state.Endpoints == nil {
		b.state.Endpoints = map[string]endpointBudget{}
	}
	b.persist = func(state requestBudgetState) error {
		encoded, err := json.Marshal(state)
		if err != nil {
			return err
		}
		_, err = s.DB.Exec(database.Upsert(s.DB.Dialect, "INSERT INTO request_budgets(scope,state) VALUES(?,?)", []string{"scope"}, []string{"state"}), scope, string(encoded))
		return err
	}
	return b, nil
}

func (b *RequestBudget) untilLocked(now time.Time) time.Time {
	cutoff := now.Add(-requestBudgetWindow).UnixMilli()
	n := 0
	for n < len(b.state.Requests) && b.state.Requests[n] <= cutoff {
		n++
	}
	b.state.Requests = b.state.Requests[n:]
	until := time.Unix(b.state.PauseUntil, 0)
	for _, e := range b.state.Endpoints {
		if e.Remaining != nil && *e.Remaining <= max(2, (e.Limit+9)/10) && e.Reset > now.Unix() {
			candidate := time.Unix(e.Reset+5, 0)
			if candidate.After(until) {
				until = candidate
			}
		}
	}
	if len(b.state.Requests) >= requestBudgetLimit {
		candidate := time.UnixMilli(b.state.Requests[len(b.state.Requests)-requestBudgetLimit]).Add(requestBudgetWindow + time.Second)
		if candidate.After(until) {
			until = candidate
		}
	}
	return until
}
func (b *RequestBudget) Until() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.untilLocked(b.now())
}
func budgetFailure(until time.Time) error {
	return &Failure{Kind: "budget", Message: "共享请求预算接近上限，已主动等待；已入库内容保留", RetryAt: until}
}
func budgetStorageFailure() error {
	return &Failure{Kind: "budget_storage", Message: "无法保存共享请求预算，已停止发送请求"}
}
func (b *RequestBudget) reserve(op string) (time.Duration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	until := b.untilLocked(now)
	if until.After(now) {
		return 0, budgetFailure(until)
	}
	if n := len(b.state.Requests); n > 0 {
		wait := time.UnixMilli(b.state.Requests[n-1]).Add(b.spacing).Sub(now)
		if wait > 0 {
			return wait, nil
		}
	}
	// Debit before dispatch. A crash or uncertain network result never refunds a request.
	b.state.Requests = append(b.state.Requests, now.UnixMilli())
	e := b.state.Endpoints[op]
	if e.Remaining != nil && e.Reset > now.Unix() {
		remaining := max(*e.Remaining-1, 0)
		e.Remaining = &remaining
		b.state.Endpoints[op] = e
	}
	if err := b.persist(b.state); err != nil {
		return 0, budgetStorageFailure()
	}
	return 0, nil
}
func (b *RequestBudget) Acquire(ctx context.Context, op string) error {
	b.pending.Add(1)
	defer b.pending.Add(-1)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		delay, err := b.reserve(op)
		if err != nil || delay == 0 {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func headerNumber(headers network.Headers, key string) (int64, bool) {
	for k, v := range headers {
		if strings.EqualFold(k, key) {
			n, err := strconv.ParseInt(strings.TrimSpace(fmt.Sprint(v)), 10, 64)
			return n, err == nil && n >= 0 && n <= 253402300799
		}
	}
	return 0, false
}
func (b *RequestBudget) Observe(op string, status int64, headers network.Headers) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	e := b.state.Endpoints[op]
	limit, hasLimit := headerNumber(headers, "x-rate-limit-limit")
	remaining, hasRemaining := headerNumber(headers, "x-rate-limit-remaining")
	reset, hasReset := headerNumber(headers, "x-rate-limit-reset")
	if hasLimit && limit > 0 {
		e.Limit = int(limit)
	}
	// Ignore stale responses from an older window; never increase remaining
	// inside the same window because responses can arrive out of order.
	if hasRemaining && (!hasReset || reset >= e.Reset) {
		if !hasReset {
			reset = e.Reset
		}
		if reset <= now.Unix() {
			reset = now.Add(requestBudgetWindow).Unix()
		}
		value := int(remaining)
		if e.Remaining != nil && reset == e.Reset {
			value = min(value, *e.Remaining)
		}
		e.Remaining, e.Reset = &value, reset
	}
	e.Status, e.Observed, e.Responses = status, now.Unix(), e.Responses+1
	b.state.Endpoints[op] = e
	reserve := max(2, (e.Limit+9)/10)
	if e.Remaining != nil && *e.Remaining <= reserve && e.Reset > now.Unix() {
		b.state.PauseUntil = max(b.state.PauseUntil, e.Reset+5)
	}
	if status == 429 {
		until := rateLimitRetryAt(headers, now)
		if !until.After(now) {
			until = now.Add(requestBudgetWindow)
		}
		b.state.PauseUntil = max(b.state.PauseUntil, until.Add(5*time.Second).Unix())
	}
	if err := b.persist(b.state); err != nil {
		return budgetStorageFailure()
	}
	available := "unknown"
	if e.Remaining != nil {
		available = strconv.Itoa(*e.Remaining)
	}
	log.Printf("x request budget operation=%s status=%d requests_in_window=%d limit=%d remaining=%s reset=%d pause_until=%d", op, status, len(b.state.Requests), e.Limit, available, e.Reset, b.state.PauseUntil)
	return nil
}
