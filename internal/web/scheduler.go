package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"xm/internal/archive"
)

type collector func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error)
type downloader func(context.Context, *archive.Store, archive.Config, string, bool) (archive.DownloadSummary, error)
type avatarDownloader func(context.Context, archive.Config, archive.Profile) error
type collectionSession interface {
	CollectBound(context.Context, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error)
	Close()
}
type sessionFactory func(context.Context, archive.Config, archive.Cookies) (collectionSession, error)

type Scheduler struct {
	app        *App
	wake       chan struct{}
	mu         sync.Mutex
	current    string
	cancel     context.CancelFunc
	active     bool
	collect    collector
	newSession sessionFactory
	download   downloader
	avatar     avatarDownloader
	lastError  string
}

func newScheduler(a *App) *Scheduler {
	return &Scheduler{app: a, wake: make(chan struct{}, 1), newSession: func(ctx context.Context, cfg archive.Config, cookies archive.Cookies) (collectionSession, error) {
		return archive.NewBrowserSession(ctx, cfg, cookies)
	}, download: archive.DownloadPending, avatar: archive.DownloadAvatar}
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
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active {
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
	cookies, err := a.store.LoadCookies()
	if err != nil {
		a.changeMu.Unlock()
		return false, nil
	}
	// Settings/import operations and claiming a job are serialized. A replacement
	// cookie or proxy always takes effect for the next job, never halfway through one.
	cookieGeneration := state(a.store.DB, "cookie_imported")
	now := time.Now().Unix()
	autoBatch := false
	if !cfg.PollingPaused {
		var due int
		if err = a.store.DB.QueryRow("SELECT COUNT(*) FROM monitors WHERE enabled=1 AND running=0 AND next_run<=?", now).Scan(&due); err != nil {
			a.changeMu.Unlock()
			return false, err
		}
		autoBatch = due > 0
	}
	condition := "m.requested=1"
	if autoBatch {
		condition = "m.enabled=1 OR m.requested=1"
	}
	rows, err := a.store.DB.Query("SELECT " + monitorColumns + " FROM monitors m WHERE m.running=0 AND (" + condition + ") ORDER BY m.requested DESC,m.username")
	if err != nil {
		a.changeMu.Unlock()
		return false, err
	}
	jobs := []Monitor{}
	for rows.Next() {
		m, scanErr := scanMonitor(rows)
		if scanErr != nil {
			rows.Close()
			a.changeMu.Unlock()
			return false, scanErr
		}
		jobs = append(jobs, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		a.changeMu.Unlock()
		return false, err
	}
	if len(jobs) == 0 {
		a.changeMu.Unlock()
		return false, nil
	}
	s.mu.Lock()
	s.active = true
	s.lastError = ""
	s.mu.Unlock()
	a.changeMu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active = false
		s.current = ""
		s.cancel = nil
		s.mu.Unlock()
	}()

	batchCtx, cancelBatch := context.WithCancel(ctx)
	defer cancelBatch()
	collect := s.collect
	var session collectionSession
	if collect == nil {
		session, err = s.newSession(batchCtx, cfg, cookies)
		if err == nil {
			collect = func(parent context.Context, _ archive.Config, _ archive.Cookies, target string, identity archive.TargetIdentity, limit int, replies bool, save func(archive.Page) error) (archive.Collection, error) {
				return session.CollectBound(parent, target, identity, limit, replies, save)
			}
		} else {
			sessionErr := err
			collect = func(context.Context, archive.Config, archive.Cookies, string, archive.TargetIdentity, int, bool, func(archive.Page) error) (archive.Collection, error) {
				return archive.Collection{}, sessionErr
			}
		}
	}
	if session != nil {
		defer session.Close()
	}

	processed := []string{}
	globalStop := false
	for _, m := range jobs {
		var enabled, requested bool
		if err = a.store.DB.QueryRow("SELECT enabled,requested FROM monitors WHERE username=?", m.Username).Scan(&enabled, &requested); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return true, err
		}
		if !enabled && !requested {
			continue
		}
		startedAt := time.Now()
		started := startedAt.Unix()
		if _, err = a.store.DB.Exec("UPDATE monitors SET running=1,requested=0,last_started=?,last_error='' WHERE username=?", started, m.Username); err != nil {
			return true, err
		}
		m.Enabled, m.Requested = enabled, requested
		log.Printf("collection start target=%s catchup=%t", m.Username, m.Catchup)
		targetCtx, cancelTarget := context.WithCancel(batchCtx)
		s.mu.Lock()
		s.current = m.Username
		s.cancel = cancelTarget
		s.mu.Unlock()
		kind, runErr := s.runMonitor(targetCtx, cfg, cookies, cookieGeneration, m, collect)
		cancelTarget()
		s.mu.Lock()
		s.current = ""
		s.cancel = nil
		s.mu.Unlock()
		processed = append(processed, m.Username)
		log.Printf("collection finish target=%s kind=%s duration=%s", m.Username, kind, time.Since(startedAt).Round(time.Millisecond))
		if runErr != nil {
			return true, runErr
		}
		if kind == "auth" || kind == "auth_replaced" || kind == "rate" {
			globalStop = true
			break
		}
	}
	if autoBatch && !globalStop {
		next := time.Now().Unix() + int64(a.Config().DefaultInterval)
		for _, target := range processed {
			if _, err = a.store.DB.Exec("UPDATE monitors SET next_run=? WHERE username=? AND enabled=1", next, target); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

func (s *Scheduler) runMonitor(jobCtx context.Context, cfg archive.Config, cookies archive.Cookies, cookieGeneration string, m Monitor, collect collector) (string, error) {
	a := s.app
	if err := a.prepareNotifications(m); err != nil {
		_, _ = a.store.DB.Exec("UPDATE monitors SET running=0 WHERE username=?", m.Username)
		return "", err
	}
	id, err := a.store.StartRun(m.Username)
	if err != nil {
		_, _ = a.store.DB.Exec("UPDATE monitors SET running=0 WHERE username=?", m.Username)
		return "", err
	}
	identity, collectErr := a.store.TargetIdentity(m.Username)
	if collectErr == nil {
		identity.KnownPostIDs, collectErr = a.store.KnownTargetPosts(m.Username, monitorCollectionLimit*4)
	}
	postAnchor, replyAnchor := m.CatchupPost, m.CatchupReply
	if collectErr == nil && !m.Catchup {
		postAnchor, replyAnchor, collectErr = a.store.TargetAnchors(m.Username)
	}
	identity.Catchup = m.Catchup
	identity.CatchupPostAnchor = postAnchor
	identity.CatchupReplyAnchor = replyAnchor
	identity.CatchupScrollBudget = m.CatchupScrolls
	var result archive.Collection
	var latestProfile *archive.Profile
	if collectErr == nil {
		result, collectErr = collect(jobCtx, cfg, cookies, m.Username, identity, monitorCollectionLimit, m.Replies, func(p archive.Page) error {
			if p.Profile != nil {
				if err := a.store.SaveTargetProfile(m.Username, *p.Profile); err != nil {
					return err
				}
				profile := *p.Profile
				latestProfile = &profile
			}
			return a.store.SavePosts(m.Username, p.Posts)
		})
	}
	if jobCtx.Err() == nil && latestProfile != nil && latestProfile.AvatarURL != "" {
		// An unavailable avatar is cosmetic and must never fail an otherwise
		// successful archive run; the UI keeps its generated fallback.
		_ = s.avatar(jobCtx, cfg, *latestProfile)
	}

	var mediaErr error
	if jobCtx.Err() == nil {
		_, mediaErr = s.download(jobCtx, a.store, cfg, m.Username, false)
	}
	ended := time.Now().Unix()
	status, message := "bounded_complete", result.Stop
	bounded := result.Stop == "limit_reached" || result.Stop == "max_scrolls_reached"
	complete := collectErr == nil && !bounded
	if collectErr == nil && bounded {
		status = "incomplete"
		message = incompleteCollectionMessage
	}
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
		return "", err
	}
	failures := 0
	if failure != nil {
		failures = m.Failures + 1
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	if archive.FailureKind(collectErr) == "auth" && state(a.store.DB, "cookie_imported") == cookieGeneration {
		if err = setState(a.store.DB, "cookie_blocked", message); err != nil {
			return "", err
		}
	}
	if archive.FailureKind(collectErr) == "access" || archive.FailureKind(collectErr) == "identity" {
		if _, err = a.store.DB.Exec("UPDATE monitors SET enabled=0,requested=0 WHERE username=?", m.Username); err != nil {
			return "", err
		}
	}
	if archive.FailureKind(collectErr) == "rate" {
		if err = setState(a.store.DB, "cooldown_until", strconv.FormatInt(ended+900, 10)); err != nil {
			return "", err
		}
	}
	var count int
	_ = a.store.DB.QueryRow("SELECT COUNT(*) FROM target_posts WHERE target=?", m.Username).Scan(&count)
	lastError := ""
	if failure != nil {
		lastError = message
	} else if !complete {
		lastError = incompleteCollectionMessage
	}
	catchup := !complete
	catchupScrolls := m.CatchupScrolls
	if catchup {
		catchupScrolls = min(max(catchupScrolls+cfg.MaxScrolls, cfg.MaxScrolls), max(1000-cfg.MaxScrolls, 0))
	} else {
		postAnchor, replyAnchor, catchupScrolls = "", "", 0
	}
	_, err = a.store.DB.Exec(`UPDATE monitors SET running=0,last_finished=?,last_success=CASE WHEN ? THEN ? ELSE last_success END,last_error=?,failures=?,last_posts=?,new_posts=?,catchup=?,catchup_post_anchor=?,catchup_reply_anchor=?,catchup_scroll_budget=? WHERE username=?`, ended, complete && jobCtx.Err() == nil, ended, lastError, failures, result.Posts, max(count-m.Posts, 0), catchup, postAnchor, replyAnchor, catchupScrolls, m.Username)

	if err != nil {
		return "", err
	}
	kind := ""
	batchKind := ""
	if collectErr != nil {
		kind = archive.FailureKind(collectErr)
		if kind == "" {
			kind = "transient"
		}
		batchKind = kind
	} else if mediaErr != nil {
		kind = "media"
	}
	if kind == "auth" && state(a.store.DB, "cookie_imported") != cookieGeneration {
		kind = ""
		batchKind = "auth_replaced"
	}
	err = a.recordNotifications(cfg, m, complete && failure == nil, jobCtx.Err() != nil, kind, failures)
	return batchKind, err
}

func (s *Scheduler) queue(target string) error {
	a := s.app
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	if _, err := a.store.LoadCookies(); err != nil {
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
