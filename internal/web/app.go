package web

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"xm/internal/archive"
	"xm/internal/database"
)

//go:embed static/*
var static embed.FS

type App struct {
	store        *archive.Store
	configPath   string
	configMu     sync.RWMutex
	config       archive.Config
	changeMu     sync.Mutex
	auth         *auth
	scheduler    *Scheduler
	notifier     *Notifier
	checkChrome  func(context.Context, archive.Config, *archive.Cookies) (archive.ChromeInfo, error)
	cacheLockMu  sync.Mutex
	cacheLocks   map[string]*cacheEntryLock
	cachePruneMu sync.Mutex
}

func New(path string, cfg archive.Config, s *archive.Store, secure bool) (*App, error) {
	if err := migrate(s); err != nil {
		return nil, err
	}
	auth, err := newAuth(s, secure)
	if err != nil {
		return nil, err
	}
	cfg, err = s.LoadSettings(cfg)
	if err != nil {
		return nil, err
	}
	if err = upgradeRateLimitState(s.DB, cfg.DefaultInterval, cfg.RateLimitMode, time.Now().Unix()); err != nil {
		return nil, err
	}
	// All enabled accounts share one batch schedule. Preserve the earliest due
	// time so restarting the service never postpones an already planned run.
	var earliest sql.NullInt64
	if err = s.DB.QueryRow("SELECT MIN(next_run) FROM monitors WHERE enabled=1").Scan(&earliest); err != nil {
		return nil, err
	}
	if earliest.Valid {
		if _, err = s.DB.Exec("UPDATE monitors SET next_run=? WHERE enabled=1", earliest.Int64); err != nil {
			return nil, err
		}
	}
	a := &App{store: s, configPath: path, config: cfg, auth: auth, checkChrome: archive.CheckChrome}
	a.scheduler = newScheduler(a)
	a.notifier = newNotifier(a)
	return a, nil
}
func (a *App) Config() archive.Config {
	a.configMu.RLock()
	defer a.configMu.RUnlock()
	cfg := a.config
	cfg.NotificationChannels = append([]archive.NotificationChannel(nil), cfg.NotificationChannels...)
	cfg.StorageProfiles = append([]archive.StorageProfile(nil), cfg.StorageProfiles...)
	return cfg
}

// CookieImported is also called by the CLI while holding the worker lock.
func CookieImported(cfg archive.Config) error {
	s, err := archive.OpenConfiguredStore(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	if err = migrate(s); err != nil {
		return err
	}
	for _, key := range []string{"cookie_blocked", "cookie_api_attempted", "cookie_api_error"} {
		if err = setState(s.DB, key, ""); err != nil {
			return err
		}
	}
	return setState(s.DB, "cookie_imported", time.Now().UTC().Format(time.RFC3339Nano))
}
func (a *App) saveConfig(cfg archive.Config, updates ...func(*database.Tx) error) error {
	old := a.Config()
	if !reflect.DeepEqual(old.Database, cfg.Database) || old.ActiveStorage() != cfg.ActiveStorage() || !reflect.DeepEqual(old.StorageProfiles, cfg.StorageProfiles) {
		return errors.New("初始化已完成，数据库与存储配置已锁定")
	}

	if err := a.store.SaveSettings(cfg, updates...); err != nil {
		return err
	}
	// Publish only after settings and all related state have committed.
	a.configMu.Lock()
	a.config = cfg
	a.configMu.Unlock()
	return nil
}
func (a *App) Run(ctx context.Context) error {
	// A process-wide worker file lock is held by the serve command at this point.
	_, err := a.store.DB.Exec("UPDATE monitors SET running=0,next_run=?,last_error='上次采集中断，等待重新执行' WHERE running=1", time.Now().Unix()+10)
	if err == nil {
		_, err = a.store.DB.Exec("UPDATE runs SET status='interrupted',finished_at=?,message='服务停止时任务尚未完成，已保存数据保留' WHERE status='running'", time.Now().UTC().Format(time.RFC3339Nano))
	}
	if err == nil {
		now := time.Now()
		_, err = a.store.DB.Exec("UPDATE collection_jobs SET status='queued',queued_at=?,queue_order=?,message='服务重启后继续执行' WHERE status='running' AND finished_at=0", now.Unix(), now.UnixNano())
	}
	if err != nil {
		return err
	}
	done := make(chan struct{})
	go func() { defer close(done); a.notifier.Run(ctx) }()
	a.scheduler.Run(ctx)
	<-done
	return nil
}

func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, status int, message string) {
	respond(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		problem(w, 415, "请求必须使用 JSON")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		problem(w, 400, "请求格式不正确或内容过大")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		problem(w, 400, "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}
func sameOrigin(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Host, r.Host) && r.Header.Get("Sec-Fetch-Site") != "cross-site"
}
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := a.store.DB.PingContext(ctx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /api/setup/status", func(w http.ResponseWriter, r *http.Request) {
		respond(w, 200, map[string]bool{"needed": false})
	})
	mux.HandleFunc("POST /api/login", a.auth.login)
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		s, _ := a.auth.session(r)
		respond(w, 200, map[string]string{"csrf": s.CSRF})
	})
	mux.HandleFunc("POST /api/logout", a.auth.logout)
	mux.HandleFunc("GET /api/overview", a.overview)
	mux.HandleFunc("GET /api/settings", a.settings)
	mux.HandleFunc("PUT /api/settings", a.updateSettings)
	mux.HandleFunc("POST /api/rate-limit/reset", a.resetRateLimit)
	mux.HandleFunc("GET /api/notifications", a.notificationSettings)
	mux.HandleFunc("POST /api/notifications/channels", a.saveNotificationChannel)
	mux.HandleFunc("DELETE /api/notifications/channels/{id}", a.deleteNotificationChannel)
	mux.HandleFunc("POST /api/notifications/channels/{id}/test", a.testNotificationChannel)
	mux.HandleFunc("POST /api/notifications/deliveries/{id}/retry", a.retryNotification)
	mux.HandleFunc("GET /api/storage", a.storageSettings)
	mux.HandleFunc("PUT /api/storage/cache", a.updateRemoteMediaCache)
	mux.HandleFunc("DELETE /api/storage/cache", a.clearRemoteMediaCache)
	mux.HandleFunc("POST /api/cookies", a.importCookie)
	mux.HandleFunc("GET /api/cookies/source", a.cookieSource)
	mux.HandleFunc("PUT /api/cookies/source", a.saveCookieSource)
	mux.HandleFunc("POST /api/cookies/fetch", a.fetchCookie)
	mux.HandleFunc("POST /api/proxy-test", a.testProxy)
	mux.HandleFunc("POST /api/chrome-test", a.testChrome)
	mux.HandleFunc("GET /api/monitors", func(w http.ResponseWriter, r *http.Request) {
		v, err := monitors(a.store.DB)
		if err != nil {
			problem(w, 500, "读取监控列表失败")
			return
		}
		_, queue, err := activityJobs(a.store.DB)
		if err != nil {
			problem(w, 500, "读取排队状态失败")
			return
		}
		for i := range v {
			for j := range queue {
				if queue[j].Target == v[i].Username {
					v[i].QueuedJob = &queue[j]
					break
				}
			}
		}
		respond(w, 200, v)
	})
	mux.HandleFunc("POST /api/monitors", a.addMonitor)
	mux.HandleFunc("PUT /api/monitors/{username}", a.editMonitor)
	mux.HandleFunc("DELETE /api/monitors/{username}", a.deleteMonitor)
	mux.HandleFunc("POST /api/monitors/{username}/run", a.runMonitor)
	mux.HandleFunc("POST /api/monitors/{username}/stop", a.stopMonitor)
	mux.HandleFunc("POST /api/monitors/{username}/reset-progress", a.resetProgress)
	mux.HandleFunc("GET /api/posts", a.posts)
	mux.HandleFunc("GET /api/targets", a.targets)
	mux.HandleFunc("GET /api/users", func(w http.ResponseWriter, r *http.Request) {
		users, err := archiveUsers(a.store.DB)
		if err != nil {
			problem(w, 500, "读取用户归档失败")
			return
		}
		dataDir := a.Config().DataDir
		for i := range users {
			if archive.AvatarExists(dataDir, users[i].UserID) {
				users[i].AvatarURL = "/avatars/" + users[i].UserID
			}
		}
		respond(w, 200, users)
	})
	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		v, err := recentRuns(a.store.DB)
		if err != nil {
			problem(w, 500, "读取记录失败")
			return
		}
		respond(w, 200, v)
	})
	mux.HandleFunc("GET /api/activity", a.activity)
	mux.HandleFunc("DELETE /api/activity/jobs/{id}", a.cancelCollectionJob)
	mux.HandleFunc("GET /media/{post}/{asset}", a.media)
	mux.HandleFunc("GET /avatars/{user}", a.avatar)
	assets, _ := fs.Sub(static, "static")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, _ := static.ReadFile("static/index.html")
		_, _ = w.Write(b)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; media-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
		mutation := r.Method != "GET" && r.Method != "HEAD"
		if mutation && !sameOrigin(r) {
			problem(w, 403, "不允许跨站请求")
			return
		}
		if (strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/login" && r.URL.Path != "/api/setup/status") || strings.HasPrefix(r.URL.Path, "/media/") || strings.HasPrefix(r.URL.Path, "/avatars/") {
			s, ok := a.auth.session(r)
			if !ok {
				problem(w, 401, "请先登录")
				return
			}
			if mutation && (r.Header.Get("X-CSRF-Token") == "" || r.Header.Get("X-CSRF-Token") != s.CSRF) {
				problem(w, 403, "会话校验失败，请刷新页面")
				return
			}
			if mutation && strings.HasPrefix(r.URL.Path, "/api/storage/") && r.URL.Path != "/api/storage/cache" {
				problem(w, 403, "初始化已完成，数据库与存储配置已锁定，无法修改")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *App) overview(w http.ResponseWriter, r *http.Request) {
	c, err := counts(a.store.DB)
	if err != nil {
		problem(w, 500, "读取统计失败")
		return
	}
	current, lastError := a.scheduler.Current()
	cfg := a.Config()
	_, cookieErr := a.store.LoadCookies()
	respond(w, 200, map[string]any{"counts": c, "running": current, "scheduler_error": lastError, "paused": cfg.PollingPaused, "cookie_ready": cookieErr == nil, "cookie_blocked": state(a.store.DB, "cookie_blocked"), "cooldown_until": state(a.store.DB, "cooldown_until"), "server_time": time.Now().Unix()})
}

func (a *App) activity(w http.ResponseWriter, r *http.Request) {
	current, queue, err := activityJobs(a.store.DB)
	if err != nil {
		problem(w, 500, "读取任务队列失败")
		return
	}
	if current != nil {
		a.scheduler.describeProgress(current)
		done, total, countErr := a.store.TargetMediaCounts(current.Target)
		if countErr == nil {
			current.MediaDone, current.MediaTotal = &done, &total
		}
	}
	runs, err := recentRuns(a.store.DB)
	if err != nil {
		problem(w, 500, "读取采集记录失败")
		return
	}
	// A run can finish between the queue and history reads. Keep it in one
	// section for this response; the next refresh will show its final result.
	if current != nil {
		filtered := runs[:0]
		for _, run := range runs {
			if int64(run["id"].(int)) != current.RunID {
				filtered = append(filtered, run)
			}
		}
		runs = filtered
	}
	now := time.Now().Unix()
	waitReason, waitUntil := "", int64(0)
	if blocked := state(a.store.DB, "cookie_blocked"); blocked != "" {
		waitReason = "登录状态待处理：" + blocked
	} else if until, _ := strconv.ParseInt(state(a.store.DB, "cooldown_until"), 10, 64); until > now {
		waitReason, waitUntil = "X 请求限流，队列将在冷却结束后继续", until
	} else if intervalUntil := a.scheduler.WaitingUntil(); intervalUntil > now {
		waitReason, waitUntil = "正在等待队列任务间隔", intervalUntil
	} else if current == nil {
		ready := false
		for _, job := range queue {
			if job.RetryAt <= now {
				ready = true
			}
			if job.RetryAt > now && (waitUntil == 0 || job.RetryAt < waitUntil) {
				waitUntil = job.RetryAt
			}
		}
		if !ready && waitUntil > now {
			waitReason = "时间线分页加载异常，等待从已保存断点重试"
		} else {
			waitUntil = 0
		}
	}
	if waitReason == "" && a.Config().PollingPaused {
		waitReason = "自动轮询已暂停；手动任务仍可执行"
	}
	respond(w, 200, map[string]any{"current": current, "queue": queue, "runs": runs, "wait_reason": waitReason, "wait_until": waitUntil, "server_time": now})
}

func (a *App) cancelCollectionJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		problem(w, 400, "排队任务编号无效")
		return
	}
	a.changeMu.Lock()
	target, err := cancelWaitingCollectionJob(a.store.DB, id)
	a.changeMu.Unlock()
	if errors.Is(err, errCollectionJobNotFound) {
		problem(w, 404, err.Error())
		return
	}
	if errors.Is(err, errCollectionJobRunning) {
		problem(w, 409, "任务已经开始执行，请使用停止当前任务")
		return
	}
	if err != nil {
		problem(w, 500, "取消排队任务失败")
		return
	}
	a.scheduler.Wake()
	respond(w, 200, map[string]string{"message": "已取消 @" + target + " 的本次排队任务"})
}
func (a *App) settings(w http.ResponseWriter, r *http.Request) {
	cfg := a.Config()
	proxyLabel := "未配置 · 直连"
	if u, err := url.Parse(cfg.Proxy); err == nil && cfg.Proxy != "" {
		proxyLabel = u.Redacted()
	}
	_, err := a.store.LoadCookies()
	respond(w, 200, map[string]any{"configuration_locked": true, "database_driver": cfg.Database.DriverName(), "database_name": cfg.Database.Name, "proxy_configured": cfg.Proxy != "", "proxy_label": proxyLabel, "cookie_ready": err == nil, "cookie_blocked": state(a.store.DB, "cookie_blocked"), "cookie_imported": state(a.store.DB, "cookie_imported"), "chrome_headless": cfg.Headless, "default_interval_seconds": cfg.DefaultInterval, "queue_interval_seconds": cfg.QueueInterval, "rate_limit_mode": cfg.RateLimitMode, "polling_paused": cfg.PollingPaused, "request_timeout_seconds": cfg.RequestTimeout, "scroll_delay_seconds": cfg.ScrollDelay, "scroll_jitter_seconds": cfg.ScrollJitter, "timeline_idle_seconds": cfg.TimelineIdleSeconds, "known_boundary_posts": cfg.KnownBoundaryPosts, "max_media_bytes": cfg.MaxMediaBytes, "browser_max_age_seconds": cfg.BrowserMaxAge, "browser_max_tasks": cfg.BrowserMaxTasks, "media_retry_initial_seconds": cfg.MediaRetryInitial, "media_retry_max_seconds": cfg.MediaRetryMax, "archive_theme": cfg.ArchiveTheme, "timeline_page_size": cfg.TimelinePageSize})
}

type settingsInput struct {
	Proxy               *string `json:"proxy"`
	DefaultInterval     *int    `json:"default_interval_seconds"`
	QueueInterval       *int    `json:"queue_interval_seconds"`
	RateLimitMode       *string `json:"rate_limit_mode"`
	RequestTimeout      *int    `json:"request_timeout_seconds"`
	ScrollDelay         *int    `json:"scroll_delay_seconds"`
	ScrollJitter        *int    `json:"scroll_jitter_seconds"`
	TimelineIdleSeconds *int    `json:"timeline_idle_seconds"`
	KnownBoundaryPosts  *int    `json:"known_boundary_posts"`
	MaxMediaBytes       *int64  `json:"max_media_bytes"`
	BrowserMaxAge       *int    `json:"browser_max_age_seconds"`
	BrowserMaxTasks     *int    `json:"browser_max_tasks"`
	MediaRetryInitial   *int    `json:"media_retry_initial_seconds"`
	MediaRetryMax       *int    `json:"media_retry_max_seconds"`
	Paused              *bool   `json:"polling_paused"`
	ArchiveTheme        *string `json:"archive_theme"`
	TimelinePageSize    *int    `json:"timeline_page_size"`
}

func (a *App) updateSettings(w http.ResponseWriter, r *http.Request) {
	var v settingsInput
	if !decode(w, r, &v) {
		return
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	cfg := a.Config()
	if v.Proxy != nil {
		if len(*v.Proxy) > 2048 {
			problem(w, 400, "代理地址过长")
			return
		}
		client, err := archive.HTTPClient(*v.Proxy, time.Second)
		if err != nil {
			problem(w, 400, err.Error())
			return
		}
		client.CloseIdleConnections()
		cfg.Proxy = *v.Proxy
	}
	if v.DefaultInterval != nil {
		if *v.DefaultInterval < 3600 || *v.DefaultInterval > 604800 {
			problem(w, 400, "自动采集周期必须为 1～168 小时")
			return
		}
		cfg.DefaultInterval = *v.DefaultInterval
	}
	if v.QueueInterval != nil {
		if *v.QueueInterval < 0 || *v.QueueInterval > 3600 {
			problem(w, 400, "队列任务间隔必须为 0～3600 秒")
			return
		}
		cfg.QueueInterval = *v.QueueInterval
	}
	tuningChanged := v.ScrollDelay != nil || v.ScrollJitter != nil || v.TimelineIdleSeconds != nil || v.KnownBoundaryPosts != nil
	if v.ScrollDelay != nil {
		cfg.ScrollDelay = *v.ScrollDelay
	}
	if v.ScrollJitter != nil {
		cfg.ScrollJitter = *v.ScrollJitter
	}
	if v.TimelineIdleSeconds != nil {
		cfg.TimelineIdleSeconds = *v.TimelineIdleSeconds
	}
	if v.KnownBoundaryPosts != nil {
		cfg.KnownBoundaryPosts = *v.KnownBoundaryPosts
	}
	if tuningChanged {
		if err := archive.ValidateCollectionTuning(cfg); err != nil {
			problem(w, 400, err.Error())
			return
		}
	}
	operationalChanged := v.RateLimitMode != nil || v.RequestTimeout != nil || v.MaxMediaBytes != nil || v.BrowserMaxAge != nil || v.BrowserMaxTasks != nil || v.MediaRetryInitial != nil || v.MediaRetryMax != nil
	if v.RateLimitMode != nil {
		cfg.RateLimitMode = *v.RateLimitMode
	}
	if v.RequestTimeout != nil {
		cfg.RequestTimeout = *v.RequestTimeout
	}
	if v.MaxMediaBytes != nil {
		cfg.MaxMediaBytes = *v.MaxMediaBytes
	}
	if v.BrowserMaxAge != nil {
		cfg.BrowserMaxAge = *v.BrowserMaxAge
	}
	if v.BrowserMaxTasks != nil {
		cfg.BrowserMaxTasks = *v.BrowserMaxTasks
	}
	if v.MediaRetryInitial != nil {
		cfg.MediaRetryInitial = *v.MediaRetryInitial
	}
	if v.MediaRetryMax != nil {
		cfg.MediaRetryMax = *v.MediaRetryMax
	}
	if operationalChanged {
		if err := archive.ValidateOperationalTuning(cfg); err != nil {
			problem(w, 400, err.Error())
			return
		}
	}
	if v.Paused != nil {
		cfg.PollingPaused = *v.Paused
	}
	if v.ArchiveTheme != nil {
		if *v.ArchiveTheme != "system" && *v.ArchiveTheme != "light" && *v.ArchiveTheme != "dark" {
			problem(w, 400, "主题模式必须为跟随系统、日间或夜间")
			return
		}
		cfg.ArchiveTheme = *v.ArchiveTheme
	}
	if v.TimelinePageSize != nil {
		if *v.TimelinePageSize < 10 || *v.TimelinePageSize > 1000 {
			problem(w, 400, "每次加载推文数量必须为 10～1000")
			return
		}
		cfg.TimelinePageSize = *v.TimelinePageSize
	}
	if err := a.saveConfig(cfg, func(tx *database.Tx) error {
		if v.DefaultInterval == nil {
			return nil
		}
		next := time.Now().Unix() + int64(cfg.DefaultInterval)
		_, err := tx.Exec("UPDATE monitors SET next_run=? WHERE enabled=1", next)
		return err
	}); err != nil {
		problem(w, 500, "配置与采集计划保存失败，请检查数据库连接与写入权限后重试")
		return
	}
	a.scheduler.Wake()
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) importCookie(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Content string `json:"content"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.Content) > 1<<20 {
		problem(w, 400, "Cookie 内容超过 1 MiB")
		return
	}
	cookies, err := archive.ParseCookies([]byte(input.Content))
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	if err = a.saveImportedCookies(cookies, true, false); err != nil {
		problem(w, 500, err.Error())
		return
	}
	a.scheduler.Wake()
	respond(w, 200, map[string]string{"message": "Cookie 已明文保存到数据库；登录状态将在下一次采集时验证"})
}
func (a *App) testProxy(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Proxy *string `json:"proxy"`
	}
	if !decode(w, r, &v) {
		return
	}
	raw := a.Config().Proxy
	if v.Proxy != nil {
		raw = *v.Proxy
	}
	c, err := archive.HTTPClient(raw, 12*time.Second)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	defer c.CloseIdleConnections()
	// A fixed destination only. No cookies, arbitrary test URLs or TLS bypass.
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	req, _ := http.NewRequestWithContext(r.Context(), "GET", "https://x.com/robots.txt", nil)
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		problem(w, 502, archive.NetworkError(err).Error())
		return
	}
	resp.Body.Close()
	respond(w, 200, map[string]any{"status": resp.StatusCode, "latency_ms": time.Since(start).Milliseconds(), "message": "已收到 X 的 HTTP 响应；这仅验证连通性，不代表 Cookie 或采集权限有效"})
}

func (a *App) testChrome(w http.ResponseWriter, r *http.Request) {
	type step struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	start := time.Now()
	cookies, cookieErr := a.store.LoadCookies()
	var cookieInput *archive.Cookies
	if cookieErr == nil {
		cookieInput = &cookies
	}
	info, checkErr := a.checkChrome(ctx, a.Config(), cookieInput)
	mode := "有界面"
	if info.Headless {
		mode = "Headless"
	}
	steps := make([]step, 0, 4)
	stage := archive.ChromeCheckStage(checkErr)
	if info.Product != "" {
		steps = append(steps, step{Name: "Chrome 环境", Status: "passed", Message: info.Product + " · " + mode + " 模式启动成功"})
	} else if stage == "chrome" {
		steps = append(steps, step{Name: "Chrome 环境", Status: "failed", Message: checkErr.Error()})
	} else {
		steps = append(steps, step{Name: "Chrome 环境", Status: "skipped", Message: "代理初始化失败，尚未启动 Chrome"})
	}
	if cookieErr != nil {
		steps = append(steps, step{Name: "Cookie", Status: "failed", Message: "未找到可用的 auth_token 和 ct0，请先在 X 账号设置中导入 Cookie"})
	} else if stage == "cookie" {
		steps = append(steps, step{Name: "Cookie", Status: "failed", Message: checkErr.Error()})
	} else {
		steps = append(steps, step{Name: "Cookie", Status: "passed", Message: "已从数据库读取 auth_token 和 ct0"})
	}
	if cookieErr != nil || stage == "chrome" || stage == "cookie" {
		steps = append(steps, step{Name: "X 网络", Status: "skipped", Message: "前置检查未通过，未连接 X"})
		steps = append(steps, step{Name: "X 登录状态", Status: "skipped", Message: "前置检查未通过，未验证登录状态"})
	} else if stage == "network" {
		steps = append(steps, step{Name: "X 网络", Status: "failed", Message: checkErr.Error()})
		steps = append(steps, step{Name: "X 登录状态", Status: "skipped", Message: "网络检查未通过，未验证登录状态"})
	} else if stage == "session" {
		steps = append(steps, step{Name: "X 网络", Status: "passed", Message: "Chrome 已打开 X 页面"})
		steps = append(steps, step{Name: "X 登录状态", Status: "failed", Message: checkErr.Error()})
	} else {
		steps = append(steps, step{Name: "X 网络", Status: "passed", Message: "Chrome 已打开 X 页面"})
		steps = append(steps, step{Name: "X 登录状态", Status: "passed", Message: "Cookie 登录状态有效，已检测到登录界面"})
	}
	respond(w, 200, map[string]any{"ok": checkErr == nil && cookieErr == nil, "latency_ms": time.Since(start).Milliseconds(), "steps": steps})
}

type monitorInput struct {
	Username string `json:"username"`
	Replies  bool   `json:"replies"`
	Enabled  bool   `json:"enabled"`
}

func (a *App) addMonitor(w http.ResponseWriter, r *http.Request) {
	var input monitorInput
	if !decode(w, r, &input) {
		return
	}
	m := Monitor{Username: input.Username, Replies: input.Replies, Enabled: input.Enabled}
	if err := validateMonitor(&m); err != nil {
		problem(w, 400, err.Error())
		return
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	var existing string
	e := a.store.DB.QueryRow("SELECT m.username FROM monitors m JOIN target_identities i ON i.target=m.username WHERE i.current_username=? LIMIT 1", m.Username).Scan(&existing)
	if e == nil {
		problem(w, 409, "此账号已在监控列表中，改名后会沿用原监控")
		return
	}
	if !errors.Is(e, sql.ErrNoRows) {
		problem(w, 500, "无法检查已有监控")
		return
	}

	repliesBaseline := "not_enabled"
	if m.Replies {
		repliesBaseline = "not_started"
	}
	_, err := a.store.DB.Exec("INSERT INTO monitors(username,replies,enabled,next_run,replies_baseline_status) VALUES(?,?,?,?,?)", m.Username, m.Replies, m.Enabled, time.Now().Unix(), repliesBaseline)
	if err != nil {
		problem(w, 409, "添加失败，此账号可能已经在监控列表中")
		return
	}
	a.scheduler.Wake()
	respond(w, 201, map[string]bool{"ok": true})
}
func (a *App) editMonitor(w http.ResponseWriter, r *http.Request) {
	var input monitorInput
	if !decode(w, r, &input) {
		return
	}
	m := Monitor{Username: r.PathValue("username"), Replies: input.Replies, Enabled: input.Enabled}
	if err := validateMonitor(&m); err != nil {
		problem(w, 400, err.Error())
		return
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	tx, err := a.store.DB.Begin()
	if err != nil {
		problem(w, 500, "修改失败")
		return
	}
	defer tx.Rollback()
	var oldReplies bool
	if err = tx.QueryRow("SELECT replies FROM monitors WHERE username=?", m.Username).Scan(&oldReplies); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			problem(w, 404, "监控账号不存在")
		} else {
			problem(w, 500, "读取监控配置失败")
		}
		return
	}
	_, err = tx.Exec(`UPDATE monitors SET replies=?,next_run=CASE WHEN enabled=0 AND ? THEN ? ELSE next_run END,enabled=?,requested=CASE WHEN ? THEN requested ELSE 0 END WHERE username=?`, m.Replies, m.Enabled, time.Now().Unix(), m.Enabled, m.Enabled, m.Username)
	if err == nil && oldReplies != m.Replies {
		err = resetMonitorBaseline(tx, m.Username)
		if err == nil {
			_, err = tx.Exec("UPDATE collection_jobs SET mode='backfill' WHERE target=? AND finished_at=0 AND status<>'running' AND mode='incremental'", m.Username)
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		problem(w, 500, "修改失败")
		return
	}
	if !m.Enabled {
		if err = cancelMonitorJobs(a.store.DB, m.Username, "监控已暂停，排队任务已取消"); err != nil {
			problem(w, 500, "取消排队任务失败")
			return
		}
		a.scheduler.Cancel(m.Username)
	}
	a.scheduler.Wake()
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) deleteMonitor(w http.ResponseWriter, r *http.Request) {
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	target := r.PathValue("username")
	current, _ := a.scheduler.Current()
	if current == target {
		problem(w, 409, "请先停止当前采集，再移除账号")
		return
	}
	res, err := a.store.DB.Exec("DELETE FROM monitors WHERE username=?", target)
	if err != nil {
		problem(w, 500, "移除失败")
		return
	}
	if err = missing(res); err != nil {
		problem(w, 404, err.Error())
		return
	}
	if err = cancelMonitorJobs(a.store.DB, target, "监控已移除，排队任务已取消"); err != nil {
		problem(w, 500, "取消排队任务失败")
		return
	}
	if _, err = a.store.DB.Exec("DELETE FROM collection_progress WHERE target=?", target); err != nil {
		problem(w, 500, "清理分页断点失败")
		return
	}
	respond(w, 200, map[string]string{"message": "已移除监控，归档内容保留"})
}

func (a *App) resetProgress(w http.ResponseWriter, r *http.Request) {
	target, err := archive.Username(r.PathValue("username"))
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	var running bool
	if err = a.store.DB.QueryRow("SELECT running FROM monitors WHERE username=?", target).Scan(&running); err != nil {
		problem(w, 404, "监控账号不存在")
		return
	}
	if running {
		problem(w, 409, "请先停止当前采集，再重置断点")
		return
	}
	tx, err := a.store.DB.Begin()
	if err != nil {
		problem(w, 500, "重置断点失败")
		return
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM collection_progress WHERE target=?", target); err == nil {
		_, err = tx.Exec("UPDATE monitors SET requested=1,last_error='',catchup_scroll_budget=0 WHERE username=?", target)
	}
	if err == nil {
		_, err = enqueueCollectionJob(tx, target, "manual", collectionModeBackfill, "重置断点后重试")
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		problem(w, 500, "重置断点失败")
		return
	}
	a.scheduler.Wake()
	respond(w, 200, map[string]string{"message": "断点已重置，已归档内容保留；任务仍遵守限流冷却"})
}
func (a *App) runMonitor(w http.ResponseWriter, r *http.Request) {
	input := struct {
		Mode string `json:"mode"`
	}{}
	if r.ContentLength != 0 && !decode(w, r, &input) {
		return
	}
	if err := a.scheduler.queue(r.PathValue("username"), input.Mode); err != nil {
		problem(w, 409, err.Error())
		return
	}
	respond(w, 202, map[string]bool{"queued": true})
}
func (a *App) stopMonitor(w http.ResponseWriter, r *http.Request) {
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	target := r.PathValue("username")
	res, err := a.store.DB.Exec("UPDATE monitors SET requested=0,enabled=0 WHERE username=?", target)
	if err != nil {
		problem(w, 500, "停止失败")
		return
	}
	if err = a.missingMonitor(res, target); err != nil {
		problem(w, 404, err.Error())
		return
	}
	if err = cancelMonitorJobs(a.store.DB, target, "任务已由用户取消"); err != nil {
		problem(w, 500, "取消排队任务失败")
		return
	}
	a.scheduler.Cancel(target)
	respond(w, 200, map[string]string{"message": "已暂停监控并请求停止当前任务"})
}
func (a *App) posts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	if offset > 10000000 {
		problem(w, 400, "分页超出范围")
		return
	}
	if len(q.Get("q")) > 500 {
		problem(w, 400, "搜索内容过长")
		return
	}
	switch q.Get("section") {
	case "", "posts", "replies", "media":
	default:
		problem(w, 400, "无效的动态类型")
		return
	}
	v, err := listPosts(a.store.DB, q.Get("target"), q.Get("q"), q.Get("kind"), q.Get("section"), offset, a.Config().TimelinePageSize)
	if err != nil {
		problem(w, 500, "读取推文失败")
		return
	}
	respond(w, 200, v)
}
func (a *App) targets(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.DB.Query("SELECT DISTINCT target FROM target_posts ORDER BY target")
	if err != nil {
		problem(w, 500, "读取账号失败")
		return
	}
	defer rows.Close()
	v := []string{}
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			problem(w, 500, "读取账号失败")
			return
		}
		v = append(v, s)
	}
	respond(w, 200, v)
}
func (a *App) media(w http.ResponseWriter, r *http.Request) {
	var path, storageID, objectKey, hash string
	var size int64
	err := a.store.DB.QueryRow("SELECT local_path,storage_id,object_key,bytes,sha256 FROM media WHERE post_id=? AND media_id=? AND status='done'", r.PathValue("post"), r.PathValue("asset")).Scan(&path, &storageID, &objectKey, &size, &hash)
	if err == nil && storageID != "local" {
		a.remoteMedia(w, r, storageID, objectKey, size, hash)
		return
	}
	if err != nil || !strings.HasPrefix(path, "media/") {
		http.NotFound(w, r)
		return
	}
	root, err := os.OpenRoot(filepath.Join(a.Config().DataDir, "media"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	f, err := root.Open(strings.TrimPrefix(path, "media/"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif", ".mp4":
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mime.TypeByExtension(ext))
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
}

func (a *App) avatar(w http.ResponseWriter, r *http.Request) {
	name, err := archive.AvatarFilename(r.PathValue("user"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	root, err := os.OpenRoot(filepath.Join(a.Config().DataDir, "avatars"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	var header [512]byte
	n, err := f.Read(header[:])
	if err != nil && !errors.Is(err, io.EOF) {
		http.NotFound(w, r)
		return
	}
	contentType := http.DetectContentType(header[:n])
	switch contentType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		http.NotFound(w, r)
		return
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, "avatar", st.ModTime(), f)
}
