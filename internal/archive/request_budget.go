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
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/network"
	"xm/internal/database"
)

const requestCohortWindow = 50 * time.Millisecond
const requestCohortJoinWindow = 6 * time.Second

type endpointBudget struct {
	Limit     int   `json:"limit"`
	Remaining *int  `json:"remaining,omitempty"`
	Reset     int64 `json:"reset"`
	Status    int64 `json:"status"`
	Observed  int64 `json:"observed"`
	Responses int64 `json:"responses"`
}
type requestBudgetState struct {
	Requests    []int64                   `json:"requests,omitempty"` // legacy rolling-budget timestamps
	LastRequest int64                     `json:"last_request_ns,omitempty"`
	Endpoints   map[string]endpointBudget `json:"endpoints"`
	PauseUntil  int64                     `json:"pause_until,omitempty"` // legacy proactive wait
}
type RequestBudget struct {
	pending        atomic.Int64
	mu             sync.Mutex
	state          requestBudgetState
	persist        func(requestBudgetState) error
	now            func() time.Time
	spacing        time.Duration
	jitter         time.Duration
	random         func(int64) int64
	scheduledAfter int64
	nextRequest    time.Time
	cohortUntil    time.Time
}

func (s *Store) LoadRequestBudget(cookies Cookies) (*RequestBudget, error) {
	digest := sha256.Sum256([]byte(cookies.AuthToken))
	scope := hex.EncodeToString(digest[:])
	// Load once per batch (also used by CLI collections). The persisted request
	// timestamps preserve spacing across tasks and browser/service restarts.
	interval := DefaultConfig().RequestInterval
	jitter := DefaultConfig().RequestJitter
	err := s.DB.QueryRow("SELECT request_interval_seconds,request_jitter_seconds FROM system_settings WHERE id=1").Scan(&interval, &jitter)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("读取请求间隔失败，已停止采集")
	}
	if err = ValidateRequestInterval(interval); err != nil {
		return nil, err
	}
	if err = ValidateRequestJitter(interval, jitter); err != nil {
		return nil, err
	}
	b := &RequestBudget{now: time.Now, spacing: time.Duration(interval) * time.Second, jitter: time.Duration(jitter) * time.Second, random: rand.Int64N}
	var raw string
	err = s.DB.QueryRow("SELECT state FROM request_budgets WHERE scope=?", scope).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal([]byte(raw), &b.state); err != nil {
			return nil, errors.New("请求间隔状态损坏，已停止采集")
		}
	}
	if b.state.Endpoints == nil {
		b.state.Endpoints = map[string]endpointBudget{}
	}
	if b.state.LastRequest == 0 && len(b.state.Requests) > 0 {
		b.state.LastRequest = time.UnixMilli(b.state.Requests[len(b.state.Requests)-1]).Add(time.Millisecond).UnixNano()
	}
	// Previous versions persisted rolling quota and proactive-pause state. Keep
	// only the last dispatch time needed for request spacing.
	b.state.Requests = nil
	b.state.PauseUntil = 0
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

func (b *RequestBudget) randomizedSpacingLocked() time.Duration {
	if b.jitter <= 0 {
		return b.spacing
	}
	seconds := int64(b.jitter / time.Second)
	return b.spacing + time.Duration(b.random(seconds*2+1)-seconds)*time.Second
}

func (b *RequestBudget) nextRequestLocked() time.Time {
	if b.state.LastRequest == 0 {
		return time.Time{}
	}
	if b.scheduledAfter != b.state.LastRequest {
		b.scheduledAfter = b.state.LastRequest
		b.nextRequest = time.Unix(0, b.state.LastRequest).Add(b.randomizedSpacingLocked())
	}
	return b.nextRequest
}

func budgetStorageFailure() error {
	return &Failure{Kind: "budget_storage", Message: "无法保存请求间隔状态，已停止发送请求"}
}
func (b *RequestBudget) reserve(op string) (time.Duration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if b.state.LastRequest != 0 {
		wait := b.nextRequestLocked().Sub(now)
		if wait > 0 {
			return wait, nil
		}
	}
	return 0, b.recordLocked(op, now)
}

func (b *RequestBudget) recordLocked(op string, now time.Time) error {
	// Persist before dispatch so a restart cannot bypass the configured spacing.
	b.state.LastRequest = now.UnixNano()
	b.scheduledAfter = 0
	if err := b.persist(b.state); err != nil {
		return budgetStorageFailure()
	}
	return nil
}

func (b *RequestBudget) acquireDeadline() (deadline time.Time, joined bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if b.cohortUntil.After(now) {
		return now, true, nil
	}
	deadline = b.nextRequestLocked()
	if deadline.IsZero() || !deadline.After(now) {
		// Give requests started by the same page action a brief chance to join the
		// same cohort. They must not be serialized for several seconds while the X
		// client is waiting, because it cancels the later paused requests.
		deadline = now.Add(requestCohortWindow)
	}
	return deadline, false, nil
}

func (b *RequestBudget) admitAtDeadline(op string, openCohort bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if openCohort && !b.cohortUntil.After(now) {
		b.cohortUntil = now.Add(requestCohortJoinWindow)
	}
	return b.recordLocked(op, now)
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

// AcquireBrowser preserves pacing between page-load cohorts while allowing the
// profile and first timeline requests created by one navigation to travel
// together. Holding those dependent requests for a second full interval causes
// the X client to cancel the paused timeline request.
func (b *RequestBudget) AcquireBrowser(ctx context.Context, op string) error {
	b.pending.Add(1)
	defer b.pending.Add(-1)
	deadline, joined, err := b.acquireDeadline()
	if err != nil {
		return err
	}
	if delay := deadline.Sub(b.now()); delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	// Keep the deadline selected when this request joined the cohort. Another
	// member may have updated LastRequest moments earlier; recalculating here
	// would split concurrent page requests into separate long waits.
	return b.admitAtDeadline(op, !joined)
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
		value := int(remaining)
		if e.Remaining != nil && reset == e.Reset {
			value = min(value, *e.Remaining)
		}
		e.Remaining, e.Reset = &value, reset
	}
	e.Status, e.Observed, e.Responses = status, now.Unix(), e.Responses+1
	b.state.Endpoints[op] = e
	if err := b.persist(b.state); err != nil {
		return budgetStorageFailure()
	}
	available := "unknown"
	if e.Remaining != nil {
		available = strconv.Itoa(*e.Remaining)
	}
	log.Printf("x request pacing operation=%s status=%d limit=%d remaining=%s reset=%d", op, status, e.Limit, available, e.Reset)
	return nil
}
