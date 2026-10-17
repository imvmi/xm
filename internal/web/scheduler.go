package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"xm/internal/archive"
)

type collector func(context.Context, archive.Config, archive.Cookies, string, int, bool, func(archive.Page) error) (archive.Collection, error)
type downloader func(context.Context, *archive.Store, archive.Config, string, bool) (archive.DownloadSummary, error)

type Scheduler struct {
	app       *App
	wake      chan struct{}
	mu        sync.Mutex
	current   string
	cancel    context.CancelFunc
	collect   collector
	download  downloader
	lastError string
}

func newScheduler(a *App) *Scheduler {
	return &Scheduler{app: a, wake: make(chan struct{}, 1), collect: archive.Collect, download: archive.DownloadPending}
}
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Scheduler) Current() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, s.lastError
}
func (s *Scheduler) Cancel(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == target && s.cancel != nil {
		s.cancel()
	}
}
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		worked, err := s.step(ctx)
		if err != nil {
			s.mu.Lock()
			s.lastError = "调度失败：" + safeMessage(err)
			s.mu.Unlock()
		}
		if worked && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

func (s *Scheduler) step(ctx context.Context) (bool, error) {
	a := s.app
	a.changeMu.Lock()
	cfg := a.Config()
	if current, _ := s.Current(); current != "" {
		a.changeMu.Unlock()
		return false, nil
	}
	if state(a.store.DB, "cookie_blocked") != "" {
		a.changeMu.Unlock()
		return false, nil
	}
	until, _ := strconv.ParseInt(state(a.store.DB, "cooldown_until"), 10, 64)
	if time.Now().Unix() < until {
		a.changeMu.Unlock()
		return false, nil
	}
	cookies, err := archive.LoadCookies(cfg.DataDir)
	if err != nil {
		a.changeMu.Unlock()
		return false, nil
	}
	// Settings/import operations and claiming a job are serialized. A replacement
	// cookie or proxy always takes effect for the next job, never halfway through one.
	cookieGeneration := state(a.store.DB, "cookie_imported")
	condition := "(m.requested=1 OR (m.enabled=1 AND m.next_run<=?))"
	if cfg.PollingPaused {
		condition = "m.requested=1"
	}
	args := []any{}
	if !cfg.PollingPaused {
		args = append(args, time.Now().Unix())
	}
	m, err := scanMonitor(a.store.DB.QueryRow("SELECT "+monitorColumns+" FROM monitors m WHERE m.running=0 AND "+condition+" ORDER BY m.requested DESC,m.next_run,m.username LIMIT 1", args...))
	if errors.Is(err, sql.ErrNoRows) {
		a.changeMu.Unlock()
		return false, nil
	}
	if err != nil {
		a.changeMu.Unlock()
		return false, err
	}
	if err = a.prepareNotifications(m); err != nil {
		a.changeMu.Unlock()
		return false, err
	}
	started := time.Now().Unix()
	_, err = a.store.DB.Exec("UPDATE monitors SET running=1,requested=0,last_started=?,last_error='' WHERE username=?", started, m.Username)
	if err != nil {
		a.changeMu.Unlock()
		return false, err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.current = m.Username
	s.cancel = cancel
	s.lastError = ""
	s.mu.Unlock()
	a.changeMu.Unlock()
	defer func() { cancel(); s.mu.Lock(); s.current = ""; s.cancel = nil; s.mu.Unlock() }()
	id, err := a.store.StartRun(m.Username)
	if err != nil {
		_, _ = a.store.DB.Exec("UPDATE monitors SET running=0,next_run=? WHERE username=?", started+60, m.Username)
		return true, err
	}
	result, collectErr := s.collect(jobCtx, cfg, cookies, m.Username, m.Limit, m.Replies, func(p archive.Page) error {
		if p.Profile != nil {
			if err := a.store.SaveProfile(*p.Profile); err != nil {
				return err
			}
		}
		return a.store.SavePosts(m.Username, p.Posts)
	})
	var mediaErr error
	if jobCtx.Err() == nil {
		_, mediaErr = s.download(jobCtx, a.store, cfg, m.Username, false)
	}
	ended := time.Now().Unix()
	status, message := "bounded_complete", result.Stop
	failure := collectErr
	if failure == nil {
		failure = mediaErr
	}
	if failure != nil {
		status = "failed"
		message = safeMessage(failure)
	}
	if collectErr == nil && mediaErr != nil {
		status = "media_failed"
	}
	if jobCtx.Err() != nil {
		status = "cancelled"
		message = "任务已停止；已入库内容保留"
		failure = jobCtx.Err()
	}
	if err = a.store.EndRun(id, status, result.Posts, message); err != nil {
		return true, err
	}
	delay := m.Interval
	failures := 0
	if failure != nil {
		failures = m.Failures + 1
		// Bounded exponential backoff, at least the configured interval.
		backoff := 60 << min(failures-1, 6)
		if delay < backoff {
			delay = backoff
		}
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	if archive.FailureKind(collectErr) == "auth" && state(a.store.DB, "cookie_imported") == cookieGeneration {
		if err = setState(a.store.DB, "cookie_blocked", message); err != nil {
			return true, err
		}
	}
	if archive.FailureKind(collectErr) == "access" {
		if _, err = a.store.DB.Exec("UPDATE monitors SET enabled=0,requested=0 WHERE username=?", m.Username); err != nil {
			return true, err
		}
	}
	if archive.FailureKind(collectErr) == "rate" {
		if delay < 900 {
			delay = 900
		}
		if err = setState(a.store.DB, "cooldown_until", strconv.FormatInt(ended+900, 10)); err != nil {
			return true, err
		}
	}
	// Honor edits made while the current collection was running.
	var currentInterval int
	if e := a.store.DB.QueryRow("SELECT interval_seconds FROM monitors WHERE username=?", m.Username).Scan(&currentInterval); e == nil {
		if failure == nil {
			delay = currentInterval
		} else {
			delay = max(delay, currentInterval)
		}
	}
	var count int
	_ = a.store.DB.QueryRow("SELECT COUNT(*) FROM target_posts WHERE target=?", m.Username).Scan(&count)
	lastError := ""
	if failure != nil {
		lastError = message
	}
	_, err = a.store.DB.Exec(`UPDATE monitors SET running=0,last_finished=?,last_success=CASE WHEN ? THEN ? ELSE last_success END,last_error=?,failures=?,next_run=?,last_posts=?,new_posts=? WHERE username=?`, ended, collectErr == nil && jobCtx.Err() == nil, ended, lastError, failures, ended+int64(delay), result.Posts, max(count-m.Posts, 0), m.Username)

	if err != nil {
		return true, err
	}
	kind := ""
	if collectErr != nil {
		kind = archive.FailureKind(collectErr)
		if kind == "" {
			kind = "transient"
		}
	} else if mediaErr != nil {
		kind = "media"
	}
	if kind == "auth" && state(a.store.DB, "cookie_imported") != cookieGeneration {
		kind = ""
	}
	err = a.recordNotifications(cfg, m, failure == nil, jobCtx.Err() != nil, kind, failures)
	return true, err
}

func (s *Scheduler) queue(target string) error {
	a := s.app
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	if _, err := archive.LoadCookies(a.Config().DataDir); err != nil {
		return fmt.Errorf("请先导入有效格式的 Cookie")
	}
	if state(a.store.DB, "cookie_blocked") != "" {
		return errors.New("登录状态受限，请先重新导入 Cookie")
	}
	r, err := a.store.DB.Exec("UPDATE monitors SET requested=1 WHERE username=? AND running=0 AND requested=0", target)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return errors.New("账号不存在，或已在队列/采集中")
	}
	s.Wake()
	return nil
}
