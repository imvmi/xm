package web

import (
	"context"
	"time"

	"xm/internal/archive"
)

const browserMaxAge = 6 * time.Hour
const browserMaxTasks = 100

// Owned exclusively by the serial scheduler; HTTP handlers only change settings.
type schedulerBrowser struct {
	session  collectionSession
	key      browserSessionKey
	created  time.Time
	tasks    int
	failures int
	retryAt  time.Time
}

type browserSessionKey struct {
	proxy, chrome                      string
	browserMode, cdpHost               string
	cdpPort                            int
	headless                           bool
	cookies                            archive.Cookies
	timeout, scrollDelay, scrollJitter int
	idle, knownBoundary                int
}

func sessionKey(cfg archive.Config, cookies archive.Cookies) browserSessionKey {
	return browserSessionKey{proxy: cfg.Proxy, chrome: cfg.ChromePath, headless: cfg.Headless, cookies: cookies, timeout: cfg.BrowserMaxAge, scrollDelay: cfg.ScrollDelay, scrollJitter: cfg.ScrollJitter, idle: cfg.TimelineIdleSeconds, knownBoundary: cfg.KnownBoundaryPosts, browserMode: cfg.BrowserMode, cdpHost: cfg.CDPHost, cdpPort: cfg.CDPPort}
}

func (s *Scheduler) closeSession() {
	if s.browser.session != nil {
		s.browser.session.Close()
		s.browser.session = nil
	}
	s.browser.key = browserSessionKey{}
	s.browser.tasks = 0
}

func (s *Scheduler) browserFailed() error {
	s.closeSession()
	s.browser.failures = min(s.browser.failures+1, 5)
	delay := min(30*time.Second*time.Duration(1<<(s.browser.failures-1)), 5*time.Minute)
	s.browser.retryAt = time.Now().Add(delay)
	return &archive.Failure{Kind: "browser", Message: "浏览器启动或连接失败，任务与断点已保留，稍后自动重建", RetryAt: s.browser.retryAt}
}

func (s *Scheduler) collectWithSession(serviceCtx, jobCtx context.Context, cfg archive.Config, cookies archive.Cookies, target string, identity archive.TargetIdentity, limit int, replies bool, save func(archive.Page) error) (archive.Collection, error) {
	key := sessionKey(cfg, cookies)
	maxAge := time.Duration(cfg.BrowserMaxAge) * time.Second
	if maxAge <= 0 {
		maxAge = browserMaxAge
	}
	maxTasks := cfg.BrowserMaxTasks
	if maxTasks <= 0 {
		maxTasks = browserMaxTasks
	}
	if s.browser.session != nil && (s.browser.key != key || time.Since(s.browser.created) >= maxAge || s.browser.tasks >= maxTasks) {
		s.closeSession()
	}
	if jobCtx.Err() != nil {
		return archive.Collection{}, jobCtx.Err()
	}
	if s.browser.session != nil {
		if err := s.browser.session.Healthy(jobCtx); err != nil {
			if jobCtx.Err() != nil {
				return archive.Collection{}, jobCtx.Err()
			}
			// A dead idle browser can be replaced before any task request is sent.
			s.closeSession()
		}
	}
	if s.browser.session == nil {
		session, err := s.newSession(serviceCtx, cfg, cookies)
		if err != nil {
			if jobCtx.Err() != nil {
				return archive.Collection{}, jobCtx.Err()
			}
			return archive.Collection{}, s.browserFailed()
		}
		s.browser.session, s.browser.key = session, key
		s.browser.created, s.browser.tasks = time.Now(), 0
	}
	result, err := s.browser.session.CollectBound(jobCtx, target, identity, limit, replies, save)
	s.browser.tasks++
	if jobCtx.Err() != nil {
		return result, err
	}
	if err != nil && s.browser.session.Healthy(jobCtx) != nil {
		if jobCtx.Err() != nil {
			return result, jobCtx.Err()
		}
		return result, s.browserFailed()
	}
	// A live browser is sufficient evidence to reset its own restart backoff.
	// Upstream auth/rate/budget state remains independently persisted.
	s.browser.failures, s.browser.retryAt = 0, time.Time{}
	return result, err
}
