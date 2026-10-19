package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	cdppage "github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)

const (
	// Fault-containment ceilings, not user pacing controls. With the supported
	// task timeout and minimum scroll wait, normal work reaches the time budget first.
	CollectionSafetyMaxPosts   = 100000
	collectionSafetyMaxPages   = 10000
	collectionSafetyMaxScrolls = 10000
)

func validateCollectionLimit(limit int) error {
	if limit < 1 || limit > CollectionSafetyMaxPosts {
		return fmt.Errorf("单次采集条数安全上限必须为 1～%d", CollectionSafetyMaxPosts)
	}
	return nil
}

func randomizedScrollWait(cfg Config) time.Duration {
	seconds := cfg.ScrollDelay
	if cfg.ScrollJitter > 0 {
		seconds += rand.IntN(cfg.ScrollJitter*2+1) - cfg.ScrollJitter
	}
	return time.Duration(max(1, seconds)) * time.Second
}

func Username(input string) (string, error) {
	s := strings.TrimSpace(input)
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "https" || !cookieDomain(u.Hostname()) || u.User != nil || u.Port() != "" {
			return "", errors.New("目标必须为 X 用户名或 https://x.com/用户名")
		}
		s = strings.Trim(u.Path, "/")
	}
	s = strings.TrimPrefix(s, "@")
	if !usernamePattern.MatchString(s) {
		return "", errors.New("用户名只能包含 1～15 个英文字母、数字或下划线")
	}
	return strings.ToLower(s), nil
}

type Collection struct {
	Posts                  int
	Pages                  int
	Scrolls                int
	Unavailable            int
	Stop                   string
	Protected              *bool
	NeedsHeadRefresh       bool
	RefreshPostAnchor      string
	RefreshReplyAnchor     string
	HistoryComplete        bool
	PostsHistoryComplete   bool
	RepliesHistoryComplete bool
}
type responseMeta struct {
	operation string
	status    int64
	retryAt   time.Time
	cursor    string
	userID    string
	username  string
	postID    string
}
type captured struct {
	sequence uint64
	meta     responseMeta
	data     []byte
	err      error
}

type requestActivity struct {
	meta      responseMeta
	graphQL   bool
	responded bool
}

func isTimelineOperation(operation string) bool {
	switch operation {
	case "UserTweets", "UserOriginalsTimeline", "UserTweetsAndReplies", "UserRepliesTimeline":
		return true
	default:
		return false
	}
}

func retryableTimelineStatus(status int64) bool {
	switch status {
	case 408, 425, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

func timelineFailure(meta responseMeta, reasons ...string) error {
	reason := "接口返回临时错误"
	if meta.status != 0 && meta.status != 200 {
		reason = fmt.Sprintf("HTTP %d", meta.status)
	}
	if len(reasons) > 0 {
		reason = reasons[0]
	}
	return fail("timeline", fmt.Sprintf("X %s 下一页加载失败（%s），已保存分页断点并等待重试", meta.operation, reason))
}

func usesIncrementalBoundary(identity TargetIdentity) bool {
	return identity.CollectionMode == "" || identity.CollectionMode == "incremental"
}

func orderedCaptures(ctx context.Context, input <-chan captured) <-chan captured {
	output := make(chan captured)
	go func() {
		defer close(output)
		pending := map[uint64]captured{}
		var next uint64
		for {
			if item, ok := pending[next]; ok {
				select {
				case output <- item:
					delete(pending, next)
					next++
					continue
				case <-ctx.Done():
					return
				}
			}
			select {
			case item, ok := <-input:
				if !ok {
					return
				}
				pending[item.sequence] = item
			case <-ctx.Done():
				return
			}
		}
	}()
	return output
}

// BrowserSession owns one Chrome process that can collect several targets in
// sequence. Each target still gets an isolated tab and timeout, while cookies,
// the temporary browser profile and the proxy bridge are shared across batches.
type BrowserSession struct {
	cfg          Config
	cookies      Cookies
	root         context.Context
	closeBrowser context.CancelFunc
	closeAlloc   context.CancelFunc
	bridge       *BrowserProxy
	profile      string
}

func NewBrowserSession(parent context.Context, cfg Config, cookies Cookies) (*BrowserSession, error) {
	profile, err := os.MkdirTemp("", "xm-chrome-*")
	if err != nil {
		return nil, err
	}
	s := &BrowserSession{cfg: cfg, cookies: cookies, profile: profile}
	fail := func(err error) (*BrowserSession, error) {
		s.Close()
		return nil, err
	}
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.UserDataDir(profile), chromedp.Flag("headless", cfg.Headless), chromedp.Flag("disable-dev-shm-usage", true), chromedp.Flag("disable-quic", true))
	if cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(cfg.ChromePath))
	}
	if cfg.Proxy != "" {
		s.bridge, err = StartBrowserProxy(cfg.Proxy)
		if err != nil {
			return fail(err)
		}
		opts = append(opts, chromedp.ProxyServer(s.bridge.URL()))
	} else {
		opts = append(opts, chromedp.Flag("no-proxy-server", true))
	}
	alloc, closeAlloc := chromedp.NewExecAllocator(parent, opts...)
	s.closeAlloc = closeAlloc
	s.root, s.closeBrowser = chromedp.NewContext(alloc, chromedp.WithErrorf(func(string, ...any) {}))
	// Chrome's initial allocation inherits the first action context. Keep that
	// context alive for the service and bound startup with a separate watchdog.
	startupTimer := time.AfterFunc(20*time.Second, s.closeBrowser)
	err = chromedp.Do(s.root, chromedp.Navigate("about:blank"))
	startedInTime := startupTimer.Stop()
	if err != nil || !startedInTime {
		return fail(errors.New("无法启动 Chrome；请检查 chrome_path 和 Chrome 运行环境"))
	}
	return s, nil
}

// Healthy checks the local blank root tab without issuing any request to X.
func (s *BrowserSession) Healthy(parent context.Context) error {
	ctx, cancel := context.WithTimeout(s.root, 5*time.Second)
	defer cancel()
	stop := context.AfterFunc(parent, cancel)
	defer stop()
	_, err := chromedp.Run(ctx, chromedp.Evaluate[bool]("true"))
	return err
}

func (s *BrowserSession) Close() {
	if s == nil {
		return
	}
	if s.closeBrowser != nil {
		s.closeBrowser()
		s.closeBrowser = nil
	}
	if s.closeAlloc != nil {
		s.closeAlloc()
		s.closeAlloc = nil
	}
	if s.bridge != nil {
		s.bridge.Close()
		s.bridge = nil
	}
	if s.profile != "" {
		_ = os.RemoveAll(s.profile)
		s.profile = ""
	}
}

func (s *BrowserSession) CollectBound(parent context.Context, target string, identity TargetIdentity, limit int, replies bool, save func(Page) error) (Collection, error) {
	return s.collectBound(parent, target, identity, limit, replies, save, nil)
}

func (s *BrowserSession) collectBound(parent context.Context, target string, identity TargetIdentity, limit int, replies bool, save func(Page) error, setup func(context.Context) error) (Collection, error) {
	return collectIdentityWithBrowser(parent, s.cfg, s.cookies, target, identity, limit, replies, save, setup, s.root)
}

func operation(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() != "x.com" || !strings.HasPrefix(u.Path, "/i/api/graphql/") {
		return ""
	}
	parts := strings.Split(u.Path, "/")
	s := parts[len(parts)-1]
	switch s {
	case "UserByScreenName", "UserByRestId", "TweetDetail", "TweetResultByRestId", "UserTweets", "UserTweetsAndReplies", "UserRepliesTimeline", "UserOriginalsTimeline":
		return s
	}
	return ""
}

// requestSubject identifies the entity selected by a GraphQL request. X pages
// issue unrelated profile lookups in the background; an error from one of those
// requests must not fail the timeline currently being collected.
func requestSubject(raw string) (userID, username, postID string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", ""
	}
	var variables map[string]any
	if json.Unmarshal([]byte(u.Query().Get("variables")), &variables) != nil {
		return "", "", ""
	}
	value := func(keys ...string) string {
		for _, key := range keys {
			if s, ok := variables[key].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	return value("userId", "user_id"), value("screen_name", "screenName"), value("focalTweetId", "tweetId", "rest_id")
}

func relevantResponse(meta responseMeta, target, expectedID, verifiedID, currentUsername, referencePost string, resolvingPost bool) bool {
	wantedID := verifiedID
	if wantedID == "" {
		wantedID = expectedID
	}
	if meta.userID != "" && wantedID != "" && meta.userID != wantedID {
		return false
	}
	wantedUsername := currentUsername
	if wantedUsername == "" {
		wantedUsername = target
	}
	if meta.username != "" && !strings.EqualFold(meta.username, wantedUsername) {
		return false
	}
	if resolvingPost && meta.postID != "" && referencePost != "" && meta.postID != referencePost {
		return false
	}
	return true
}

// Collect reads the browser's actual GraphQL responses. No guest access, query-ID
// guessing, automatic login, or alternate-account fallback is performed.
func Collect(parent context.Context, cfg Config, cookies Cookies, target string, limit int, replies bool, save func(Page) error) (Collection, error) {
	return collect(parent, cfg, cookies, target, limit, replies, save, nil)
}

func CollectBound(parent context.Context, cfg Config, cookies Cookies, target string, identity TargetIdentity, limit int, replies bool, save func(Page) error) (Collection, error) {
	return collectIdentity(parent, cfg, cookies, target, identity, limit, replies, save, nil)
}

// Try the last verified handle first. Only an empty, unsuccessful collection
// may retry through a stable archived post; without one, retry the handle route
// instead of /i/user/{id}, whose UserByRestId lookup is not generally available.
func collectIdentity(parent context.Context, cfg Config, cookies Cookies, target string, identity TargetIdentity, limit int, replies bool, save func(Page) error, setup func(context.Context) error) (Collection, error) {
	return collectIdentityWithBrowser(parent, cfg, cookies, target, identity, limit, replies, save, setup, nil)
}

func collectIdentityWithBrowser(parent context.Context, cfg Config, cookies Cookies, target string, identity TargetIdentity, limit int, replies bool, save func(Page) error, setup func(context.Context) error, root context.Context) (result Collection, resultErr error) {
	ctx, cancel := context.WithTimeout(parent, collectionTimeout(cfg, identity))
	defer cancel()
	saveFailed := false
	defer func() {
		if identity.DurableProgress && !saveFailed && resultErr != nil && FailureKind(resultErr) == "transient" && parent.Err() == nil && ctx.Err() == context.DeadlineExceeded {
			result.Stop, resultErr = "time_budget_reached", nil
		}
	}()
	checkedSave := func(p Page) error {
		err := save(p)
		if err != nil {
			saveFailed = true
		}
		return err
	}
	var err error
	result, err = collectBoundWithBrowser(ctx, cfg, cookies, target, identity, limit, replies, checkedSave, setup, root)
	if err == nil || identity.UserID == "" || identity.CurrentUsername == "" || result.Posts > 0 || saveFailed || ctx.Err() != nil || FailureKind(err) == "auth" || FailureKind(err) == "rate" || FailureKind(err) == "timeline" || FailureKind(err) == "checkpoint" || FailureKind(err) == "budget_storage" {
		return result, err
	}
	if userIDPattern.MatchString(identity.ReferencePostID) {
		identity.CurrentUsername = ""
	}
	return collectBoundWithBrowser(ctx, cfg, cookies, target, identity, limit, replies, checkedSave, setup, root)
}

func collectionTimeout(cfg Config, _ TargetIdentity) time.Duration {
	return time.Duration(cfg.BrowserMaxAge) * time.Second
}

func collect(parent context.Context, cfg Config, cookies Cookies, target string, limit int, replies bool, save func(Page) error, setup func(context.Context) error) (Collection, error) {
	return collectBound(parent, cfg, cookies, target, TargetIdentity{}, limit, replies, save, setup)
}
func collectBound(parent context.Context, cfg Config, cookies Cookies, target string, identity TargetIdentity, limit int, replies bool, save func(Page) error, setup func(context.Context) error) (Collection, error) {
	return collectBoundWithBrowser(parent, cfg, cookies, target, identity, limit, replies, save, setup, nil)
}

func collectBoundWithBrowser(parent context.Context, cfg Config, cookies Cookies, target string, identity TargetIdentity, limit int, replies bool, save func(Page) error, setup func(context.Context) error, root context.Context) (Collection, error) {
	var result Collection
	originalReplies := replies
	expectedID := identity.UserID
	progress := identity.Progress
	result.PostsHistoryComplete = progress.Posts.Exhausted
	result.RepliesHistoryComplete = progress.Replies.Exhausted
	result.HistoryComplete = result.PostsHistoryComplete && (!originalReplies || result.RepliesHistoryComplete)
	checkpointing := identity.DurableProgress || progress.UserID != ""
	resuming := progress.Posts.Cursor != "" || progress.Replies.Cursor != "" || progress.Posts.Done || progress.Replies.Done
	finish := func() Collection {
		result.NeedsHeadRefresh = resuming
		result.RefreshPostAnchor = progress.Posts.Head
		result.RefreshReplyAnchor = progress.Replies.Head
		return result
	}
	if progress.UserID != "" && progress.UserID != expectedID {
		return result, checkpointFailure()
	}
	if progress.Replies.Done {
		replies = false
	}
	if progress.Posts.Done && (!replies || progress.Replies.Done) {
		result.Stop = "known_boundary_reached"
		return finish(), nil
	}
	if expectedID != "" && !userIDPattern.MatchString(expectedID) {
		return result, fail("identity", "无效的绑定用户 ID")
	}
	if err := validateCollectionLimit(limit); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(parent, collectionTimeout(cfg, identity))
	defer cancel()
	var browser context.Context
	var err error
	if root == nil {
		profile, err := os.MkdirTemp("", "xm-chrome-*")
		if err != nil {
			return result, err
		}
		defer os.RemoveAll(profile)
		opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
		opts = append(opts, chromedp.UserDataDir(profile), chromedp.Flag("headless", cfg.Headless), chromedp.Flag("disable-dev-shm-usage", true), chromedp.Flag("disable-quic", true))
		if cfg.ChromePath != "" {
			opts = append(opts, chromedp.ExecPath(cfg.ChromePath))
		}
		if cfg.Proxy != "" {
			bridge, e := StartBrowserProxy(cfg.Proxy)
			if e != nil {
				return result, e
			}
			defer bridge.Close()
			opts = append(opts, chromedp.ProxyServer(bridge.URL()))
		} else {
			opts = append(opts, chromedp.Flag("no-proxy-server", true))
		}
		alloc, closeAlloc := chromedp.NewExecAllocator(ctx, opts...)
		defer closeAlloc()
		var closeBrowser context.CancelFunc
		browser, closeBrowser = chromedp.NewContext(alloc, chromedp.WithErrorf(func(string, ...any) {}))
		defer closeBrowser()
	} else {
		var closeBrowser context.CancelFunc
		browser, closeBrowser = chromedp.NewContext(root)
		stop := context.AfterFunc(ctx, closeBrowser)
		defer stop()
		defer closeBrowser()
	}
	unordered := make(chan captured, 128)
	var overflow atomic.Bool
	send := func(c captured) {
		select {
		case unordered <- c:
		case <-browser.Done():
		default:
			overflow.Store(true)
		}
	}
	err = chromedp.Do(browser, chromedp.Func(func(c context.Context, t *chromedp.Target) error {
		total, resource := int64(100<<20), int64(20<<20)
		if _, e := cdp.Call(c, t, network.Enable, network.EnableParams{MaxTotalBufferSize: &total, MaxResourceBufferSize: &resource}); e != nil {
			return e
		}
		if _, e := cdp.Call(c, t, network.SetCacheDisabled, network.SetCacheDisabledParams{CacheDisabled: true}); e != nil {
			return e
		}
		if progress.Posts.Cursor != "" || progress.Replies.Cursor != "" {
			if _, e := cdp.Call(c, t, cdppage.AddScriptToEvaluateOnNewDocument, cdppage.AddScriptToEvaluateOnNewDocumentParams{Source: resumeScript(progress)}); e != nil {
				return e
			}
		}
		for _, v := range []struct{ name, value string }{{"auth_token", cookies.AuthToken}, {"ct0", cookies.CSRF}} {
			secure, httponly := true, v.name == "auth_token"
			if _, e := cdp.Call(c, t, network.SetCookie, network.SetCookieParams{Name: v.name, Value: v.value, Domain: ".x.com", Path: "/", Secure: &secure, HTTPOnly: &httponly}); e != nil {
				return e
			}
		}
		return nil
	}))
	if err != nil {
		return result, errors.New("无法启动 Chrome 或导入 Cookie；请检查 chrome_path 和 Chrome 运行环境")
	}
	if setup != nil {
		if err = setup(browser); err != nil {
			return result, err
		}
	}
	// Observe GraphQL activity without intercepting or delaying it. The scroll
	// loop uses this only to avoid starting a new page action while the previous
	// one is still loading.
	var graphQLInflight atomic.Int64
	var responseReaders atomic.Int64
	var activityMu sync.Mutex
	graphQLRequests := make(map[network.RequestID]requestActivity)
	finishedBeforeStart := make(map[network.RequestID]struct{})
	requestEvents := chromedp.Events(browser, network.RequestWillBeSent)
	finishedEvents := chromedp.Events(browser, network.LoadingFinished)
	failedEvents := chromedp.Events(browser, network.LoadingFailed)
	responses := chromedp.Events(browser, network.ResponseReceived)
	var sequenceMu sync.Mutex
	var sequence uint64
	nextSequence := func() uint64 {
		sequenceMu.Lock()
		defer sequenceMu.Unlock()
		current := sequence
		sequence++
		return current
	}
	completeRequest := func(id network.RequestID) (requestActivity, bool) {
		activityMu.Lock()
		defer activityMu.Unlock()
		activity, started := graphQLRequests[id]
		if !started {
			finishedBeforeStart[id] = struct{}{}
			return requestActivity{}, false
		}
		delete(graphQLRequests, id)
		if activity.graphQL {
			graphQLInflight.Add(-1)
		}
		return activity, true
	}
	go func() {
		for e, err := range requestEvents {
			if err != nil {
				return
			}
			if e.Request == nil {
				continue
			}
			op := operation(e.Request.URL)
			userID, username, postID := requestSubject(e.Request.URL)
			activity := requestActivity{graphQL: op != "", meta: responseMeta{operation: op, cursor: requestCursor(e.Request.URL), userID: userID, username: username, postID: postID}}
			activityMu.Lock()
			if _, finished := finishedBeforeStart[e.RequestID]; finished {
				delete(finishedBeforeStart, e.RequestID)
				activityMu.Unlock()
				continue
			}
			previous, existed := graphQLRequests[e.RequestID]
			graphQLRequests[e.RequestID] = activity
			if activity.graphQL && (!existed || !previous.graphQL) {
				graphQLInflight.Add(1)
			} else if !activity.graphQL && existed && previous.graphQL {
				graphQLInflight.Add(-1)
			}
			activityMu.Unlock()
		}
	}()
	go func() {
		for e, err := range finishedEvents {
			if err != nil {
				return
			}
			completeRequest(e.RequestID)
		}
	}()
	go func() {
		for e, err := range failedEvents {
			if err != nil {
				return
			}
			activity, started := completeRequest(e.RequestID)
			if !started || !activity.graphQL || activity.responded || e.Canceled || !isTimelineOperation(activity.meta.operation) {
				continue
			}
			// Response readers for earlier protocol events must publish their pages
			// before this failure can terminate the attempt and preserve its cursor.
			go func(meta responseMeta) {
				timer := time.NewTimer(50 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-browser.Done():
					return
				case <-timer.C:
				}
				for responseReaders.Load() > 0 {
					select {
					case <-browser.Done():
						return
					case <-time.After(10 * time.Millisecond):
					}
				}
				send(captured{sequence: nextSequence(), meta: meta, err: timelineFailure(meta, "浏览器网络请求失败")})
			}(activity.meta)
		}
	}()
	go func() {
		for e, err := range responses {
			if err != nil {
				return
			}
			if e.Response == nil {
				continue
			}
			op := operation(e.Response.URL)
			if op == "" {
				continue
			}
			activityMu.Lock()
			if activity, ok := graphQLRequests[e.RequestID]; ok {
				activity.responded = true
				graphQLRequests[e.RequestID] = activity
			}
			activityMu.Unlock()
			userID, username, postID := requestSubject(e.Response.URL)
			meta := responseMeta{operation: op, status: e.Response.Status, retryAt: rateLimitRetryAt(e.Response.Headers, time.Now()), cursor: requestCursor(e.Response.URL), userID: userID, username: username, postID: postID}
			current := nextSequence()
			responseReaders.Add(1)
			go func(id network.RequestID, meta responseMeta, sequence uint64) {
				defer responseReaders.Add(-1)
				if meta.status != 200 {
					send(captured{sequence: sequence, meta: meta})
					return
				}
				// The response event precedes body completion. Read only after CDP makes
				// the full body available, with a bounded wait and cancellation.
				readCtx, cancelRead := context.WithTimeout(browser, 30*time.Second)
				defer cancelRead()
				for {
					var body []byte
					err := chromedp.Do(readCtx, chromedp.Func(func(c context.Context, t *chromedp.Target) error {
						r, err := cdp.Call(c, t, network.GetResponseBody, network.GetResponseBodyParams{RequestID: id})
						body = r.Body
						return err
					}))
					if err == nil {
						send(captured{sequence: sequence, meta: meta, data: body})
						return
					}
					select {
					case <-readCtx.Done():
						err := errors.New("未能读取完整浏览器响应，已入库内容保留，请重试")
						if isTimelineOperation(meta.operation) {
							err = timelineFailure(meta, "浏览器响应读取超时")
						}
						send(captured{sequence: sequence, meta: meta, err: err})
						return
					case <-time.After(250 * time.Millisecond):
					}
				}
			}(e.RequestID, meta, current)
		}
	}()
	events := orderedCaptures(browser, unordered)
	path := "https://x.com/" + target
	resolvingPost := expectedID != "" && identity.CurrentUsername == "" && userIDPattern.MatchString(identity.ReferencePostID)
	if expectedID != "" {
		path = "https://x.com/i/user/" + expectedID
		if resolvingPost {
			path = "https://x.com/i/status/" + identity.ReferencePostID
		} else if identity.CurrentUsername != "" {
			if !usernamePattern.MatchString(identity.CurrentUsername) {
				return result, fail("identity", "无效的绑定用户名")
			}
			path = "https://x.com/" + identity.CurrentUsername
			if replies {
				path += "/with_replies"
			}
		}
	} else if replies {
		path += "/with_replies"
	}
	if err = chromedp.Do(browser, chromedp.Navigate(path)); err != nil {
		return result, errors.New("无法打开 X 主页，请检查代理和网络；也可能已达到采集超时")
	}
	if err = confirmSession(browser); err != nil {
		return result, err
	}
	seen := make(map[string]bool)
	verifiedID := ""
	verifiedUsername := ""
	var pending []Page
	replyNavigation := expectedID != "" && identity.CurrentUsername != "" && replies
	scrolls, totalScrolls, lastCount, lastPages := 0, 0, 0, 0
	phaseStartPages := 0
	hitScrollLimit := false
	knownStreak := 0
	newPosts := 0
	boundaryReached := false
	phaseFirstResponse := true
	resumeCursor := progress.Posts.Cursor
	if replies {
		resumeCursor = progress.Replies.Cursor
	}
	maxScrolls := collectionSafetyMaxScrolls
	pageBudget := collectionSafetyMaxPages
	knownBoundary := cfg.KnownBoundaryPosts
	if knownBoundary <= 0 {
		knownBoundary = DefaultConfig().KnownBoundaryPosts
	}
	idleSeconds := cfg.TimelineIdleSeconds
	if idleSeconds <= 0 {
		idleSeconds = DefaultConfig().TimelineIdleSeconds
	}
	idleDuration := time.Duration(idleSeconds) * time.Second
	var idleSince time.Time
	lastTop, lastHeight := -1.0, -1.0
	scrollTimer := time.NewTimer(randomizedScrollWait(cfg))
	defer scrollTimer.Stop()
	resetScrollTimerAfter := func(wait time.Duration) {
		if !scrollTimer.Stop() {
			select {
			case <-scrollTimer.C:
			default:
			}
		}
		scrollTimer.Reset(wait)
	}
	resetScrollTimer := func() {
		resetScrollTimerAfter(randomizedScrollWait(cfg))
	}
	startOriginalTimeline := func() error {
		progress.UserID = verifiedID
		if !hitScrollLimit {
			progress.Replies.Done = true
		}
		if checkpointing {
			if err := save(Page{Progress: &progress}); err != nil {
				return err
			}
		}
		replies = false
		pending = nil
		phaseStartPages = result.Pages
		scrolls, knownStreak = 0, 0
		idleSince = time.Time{}
		lastTop, lastHeight = -1, -1
		boundaryReached = false
		phaseFirstResponse = true
		resumeCursor = progress.Posts.Cursor
		return chromedp.Do(browser, chromedp.Navigate("https://x.com/"+verifiedUsername))
	}
	for {
		if overflow.Load() {
			return result, errors.New("浏览器响应队列溢出，本次采集不完整，请降低采集量")
		}
		select {
		case <-browser.Done():
			return result, errors.New("浏览器已关闭或采集超时；已入库内容保留，可重新采集")
		case c, ok := <-events:
			if !ok {
				return result, errors.New("浏览器响应流已关闭；已入库内容保留，可重新采集")
			}
			isDetail := c.meta.operation == "TweetDetail" || c.meta.operation == "TweetResultByRestId"
			if resolvingPost && !isDetail || !resolvingPost && isDetail {
				continue
			}
			isCurrentTimeline := !replies && (c.meta.operation == "UserTweets" || c.meta.operation == "UserOriginalsTimeline") || replies && (c.meta.operation == "UserTweetsAndReplies" || c.meta.operation == "UserRepliesTimeline")
			if resumeCursor != "" && phaseFirstResponse && isCurrentTimeline && c.meta.cursor != resumeCursor {
				return result, checkpointFailure()
			}
			if !relevantResponse(c.meta, target, expectedID, verifiedID, identity.CurrentUsername, identity.ReferencePostID, resolvingPost) {
				continue
			}
			if c.err != nil {
				if FailureKind(c.err) == "timeline" && !isCurrentTimeline {
					continue
				}
				return result, c.err
			}
			if isCurrentTimeline {
				// The next user-like scroll is timed from the completed timeline
				// response, not from the previous fixed clock tick.
				idleSince = time.Time{}
				resetScrollTimer()
			}
			switch c.meta.status {
			case 200:
			case 401:
				return result, fail("auth", "Cookie 登录态失效，请重新导入")
			case 403:
				return result, fail("access", "X "+c.meta.operation+" 拒绝访问：可能无查看权限、账号受限或需要浏览器验证")
			case 429:
				return result, rateLimitFailure(c.meta)
			default:
				if isCurrentTimeline && retryableTimelineStatus(c.meta.status) {
					return result, timelineFailure(c.meta)
				}
				if c.meta.cursor != "" {
					return result, checkpointFailure()
				}
				return result, fmt.Errorf("X 数据请求返回 HTTP %d", c.meta.status)
			}
			if resolvingPost {
				p, e := IdentityFromPost(c.data, identity.ReferencePostID)
				if e != nil {
					if FailureKind(e) == "rate" {
						e = rateLimitFailure(c.meta)
					}
					return result, e
				}
				if p.ID != expectedID {
					return result, fail("identity", "归档推文的作者 ID 与绑定身份不符")
				}
				resolvingPost = false
				replyNavigation = replies
				pending = nil
				next := "https://x.com/" + p.Username
				if replies {
					next += "/with_replies"
				}
				if e = chromedp.Do(browser, chromedp.Navigate(next)); e != nil {
					return result, errors.New("无法打开已核实身份的用户主页")
				}
				continue
			}
			if replies && c.meta.operation != "UserByScreenName" && c.meta.operation != "UserByRestId" && c.meta.operation != "UserTweetsAndReplies" && c.meta.operation != "UserRepliesTimeline" {
				continue
			}
			if !replies && (c.meta.operation == "UserTweetsAndReplies" || c.meta.operation == "UserRepliesTimeline") {
				continue
			}
			page, e := ParsePage(c.data, c.meta.operation)
			if e != nil {
				if FailureKind(e) == "rate" {
					e = rateLimitFailure(c.meta)
				} else if isCurrentTimeline && FailureKind(e) == "transient" {
					e = timelineFailure(c.meta, "响应解析失败或接口返回临时错误")
				}
				return result, e
			}
			if page.Timeline && phaseFirstResponse {
				if resumeCursor != "" && c.meta.cursor != resumeCursor {
					return result, checkpointFailure()
				}
				phaseFirstResponse = false
			}
			page.RequestCursor = c.meta.cursor
			if page.Profile != nil {
				p := page.Profile
				if !userIDPattern.MatchString(p.ID) || !usernamePattern.MatchString(p.Username) {
					return result, fail("identity", "无法确认目标账号身份")
				}
				if expectedID != "" && p.ID != expectedID {
					return result, fail("identity", "目标用户 ID 与绑定身份不符，已停止采集")
				}
				if expectedID == "" && !strings.EqualFold(p.Username, target) {
					continue
				}
				if verifiedID != "" && p.ID != verifiedID {
					return result, fail("identity", "采集中账号身份发生变化，已停止采集")
				}
				verifiedID = p.ID
				verifiedUsername = p.Username
				protected := p.Protected
				result.Protected = &protected
				if err = save(page); err != nil {
					return result, err
				}
				if expectedID != "" && replies && !replyNavigation {
					replyNavigation = true
					pending = nil
					if err = chromedp.Do(browser, chromedp.Navigate("https://x.com/"+p.Username+"/with_replies")); err != nil {
						return result, errors.New("无法打开回复时间线")
					}
					continue
				}
			} else if verifiedID == "" {
				if len(pending) >= 32 {
					return result, fail("identity", "未能确认用户身份，未保存时间线")
				}
				pending = append(pending, page)
				continue
			}
			pages := pending
			pending = nil
			if page.Profile == nil {
				pages = append(pages, page)
			}
			for _, page := range pages {
				truncated := false
				emptyPage := len(page.Posts) == 0
				knownPosts := identity.KnownPostIDs
				if identity.LookupKnownPosts != nil {
					ids := make([]string, 0, len(page.Posts))
					for _, p := range page.Posts {
						if p.AuthorID == verifiedID && !seen[p.ID] {
							ids = append(ids, p.ID)
						}
					}
					knownPosts, err = identity.LookupKnownPosts(ids)
					if err != nil {
						return result, err
					}
				}
				filtered := []Post{}
				for _, p := range page.Posts {
					// Author ID is authoritative. Historical posts may still
					// carry an old handle; quoted authors remain separate.
					if p.AuthorID != verifiedID || seen[p.ID] {
						continue
					}
					known := knownPosts[p.ID]
					if identity.Catchup && usesIncrementalBoundary(identity) {
						anchor := identity.CatchupPostAnchor
						if replies {
							anchor = identity.CatchupReplyAnchor
						}
						if anchor != "" && p.ID == anchor {
							boundaryReached = true
						}
					} else if usesIncrementalBoundary(identity) {
						if known {
							knownStreak++
						} else {
							knownStreak = 0
						}
					}
					if !known {
						if newPosts >= limit {
							truncated = true
							continue
						}
						newPosts++
					}
					seen[p.ID] = true
					filtered = append(filtered, p)
				}
				page.Posts = filtered
				if page.Timeline {
					progress.UserID = verifiedID
					phase := &progress.Posts
					if replies {
						phase = &progress.Replies
					}
					if phase.Head == "" && page.RequestCursor == "" {
						for _, p := range filtered {
							if len(p.ID) > len(phase.Head) || len(p.ID) == len(phase.Head) && p.ID > phase.Head {
								phase.Head = p.ID
							}
						}
					}
					if !truncated {
						phase.ReachedAt = advanceReachedAt(phase.ReachedAt, filtered)
					}
					if !truncated && page.NextCursor != "" {
						if page.NextCursor == page.RequestCursor {
							if !emptyPage {
								return result, checkpointFailure()
							}
							phase.Done = true
						}
						phase.Cursor = page.NextCursor
					}
					incrementalBoundary := usesIncrementalBoundary(identity) && !identity.Catchup && knownStreak >= knownBoundary
					if !truncated && (page.End || boundaryReached || incrementalBoundary) {
						phase.Done = true
					}
					if !truncated && page.End {
						phase.Exhausted = true
						if replies {
							result.RepliesHistoryComplete = true
						} else {
							result.PostsHistoryComplete = true
							result.HistoryComplete = !originalReplies || result.RepliesHistoryComplete
						}
					}
					if checkpointing {
						page.Progress = &progress
					}
				}
				if err = save(page); err != nil {
					return result, err
				}
				result.Posts += len(filtered)
				result.Unavailable += page.Unavailable
				if page.Timeline {
					result.Pages++
				}
				if newPosts >= limit {
					result.Stop = "limit_reached"
					return result, nil
				}
				// Check inside the loop as several responses can be buffered while
				// the profile request is still verifying this user's identity.
				if checkpointing && result.Pages >= pageBudget {
					phase := progress.Posts
					if replies {
						phase = progress.Replies
					}
					if phase.Done && !replies && !hitScrollLimit {
						result.Stop = "known_boundary_reached"
						return finish(), nil
					}
					if !phase.Done && phase.Cursor == "" {
						return result, checkpointFailure()
					}
					result.Stop = "page_budget_reached"
					return result, nil
				}
			}
			phaseDone := progress.Posts.Done
			if replies {
				phaseDone = progress.Replies.Done
			}
			if phaseDone {
				if replies {
					if err = startOriginalTimeline(); err != nil {
						return result, err
					}
					continue
				}
				if hitScrollLimit {
					result.Stop = "max_scrolls_reached"
					return result, nil
				}
				result.Stop = "known_boundary_reached"
				return finish(), nil
			}
		case <-scrollTimer.C:
			resetScrollTimer()
			// Wait for the previous page action's GraphQL activity to finish.
			// A protected background request must not be mistaken for the end of
			// a timeline or spend a scroll either.
			if graphQLInflight.Load() > 0 {
				idleSince = time.Time{}
				continue
			}
			type pageState struct {
				Path   string  `json:"path"`
				Denied bool    `json:"denied"`
				Top    float64 `json:"top"`
				Height float64 `json:"height"`
				Bottom bool    `json:"bottom"`
			}
			state, e := chromedp.Run(browser, chromedp.Evaluate[pageState](`(()=>{const s=document.scrollingElement||document.documentElement;return {path:location.pathname,denied:/These posts are protected|These Tweets are protected|Only approved followers|这些帖子受到保护|这些推文受到保护/.test(document.body.innerText),top:s.scrollTop,height:s.scrollHeight,bottom:s.scrollTop+s.clientHeight>=s.scrollHeight-32}})()`))
			if e != nil {
				return result, errors.New("无法读取浏览器状态，浏览器可能已关闭")
			}
			if strings.HasPrefix(state.Path, "/i/flow/login") || strings.HasPrefix(state.Path, "/account/access") {
				return result, fail("auth", "X 要求重新登录或人工验证，请在浏览器处理后重新导入 Cookie")
			}
			if state.Denied {
				return result, fail("access", "当前账号无权查看此锁推账号，请确认关注请求已经获准")
			}
			// Lack of responses while traversing an already loaded, tall timeline
			// is not exhaustion. Start the wall-clock idle grace period only at a
			// stable bottom, after scrolling and timeline responses stop progressing.
			stableBottom := state.Bottom && state.Top == lastTop && state.Height == lastHeight && result.Posts == lastCount && result.Pages == lastPages
			lastCount = result.Posts
			lastPages = result.Pages
			lastTop, lastHeight = state.Top, state.Height
			idleExpired := false
			if stableBottom {
				now := time.Now()
				if idleSince.IsZero() {
					idleSince = now
					resetScrollTimerAfter(idleDuration)
					continue
				}
				remaining := idleDuration - now.Sub(idleSince)
				if remaining > 0 {
					resetScrollTimerAfter(remaining)
					continue
				}
				idleExpired = true
			} else {
				idleSince = time.Time{}
			}
			if scrolls >= maxScrolls || idleExpired {
				if verifiedID == "" {
					return result, fail("identity", "未能确认目标用户 ID，未保存推文")
				}
				if result.Pages == phaseStartPages {
					if resumeCursor != "" {
						return result, checkpointFailure()
					}
					return result, errors.New("未收到可读取的用户时间线，不能认定为没有推文；请检查登录态、权限、代理或页面变化")
				}
				if result.Posts == 0 && result.Unavailable > 0 {
					return result, errors.New("时间线仅返回不可访问的内容，无法归档")
				}
				if scrolls >= maxScrolls {
					hitScrollLimit = true
					// With a durable cursor, continue this stream next time instead
					// of pretending it finished and starting another full timeline.
					if checkpointing && (progress.Posts.Cursor != "" || progress.Replies.Cursor != "") {
						result.Stop = "max_scrolls_reached"
						return result, nil
					}
				}
				if replies {
					// Modern reply timelines do not contain all original posts.
					// Spend the remaining shared budget on the verified user's main
					// timeline, retaining seen IDs to deduplicate overlapping posts.
					if err = startOriginalTimeline(); err != nil {
						return result, errors.New("回复已保存，但无法打开普通推文时间线；本轮采集不完整")
					}
					continue
				}
				result.Stop = "no_new_posts_observed"
				if !hitScrollLimit {
					progress.UserID = verifiedID
					progress.Posts.Done = true
					if checkpointing {
						if err = save(Page{Progress: &progress}); err != nil {
							return result, err
						}
					}
				}
				if hitScrollLimit {
					result.Stop = "max_scrolls_reached"
				}
				return finish(), nil
			}
			// Reach the loaded timeline's end to trigger its next page. Small fixed
			// scrolls can consume the entire budget inside a single media-heavy page.
			_, err = chromedp.Run(browser, chromedp.Evaluate[any](`(()=>{const s=document.scrollingElement||document.documentElement;s.scrollTo({top:s.scrollHeight,behavior:'instant'})})()`))
			if err != nil {
				return result, errors.New("页面滚动失败")
			}
			scrolls++
			totalScrolls++
			result.Scrolls = totalScrolls
		}
	}
}

func confirmSession(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		type state struct {
			LoggedIn bool   `json:"loggedIn"`
			Path     string `json:"path"`
		}
		s, err := chromedp.Run(ctx, chromedp.Evaluate[state](`({loggedIn:!!document.querySelector('[data-testid="SideNav_AccountSwitcher_Button"],[data-testid="AppTabBar_Profile_Link"]'),path:location.pathname})`))
		if err != nil {
			return errors.New("无法确认登录状态；请检查网络，或使用 headless=false 查看页面")
		}
		if strings.HasPrefix(s.Path, "/i/flow/login") || strings.HasPrefix(s.Path, "/account/access") {
			return fail("auth", "Cookie 失效或 X 要求人工验证，请在浏览器处理后重新导入")
		}
		if s.LoggedIn {
			return nil
		}
		select {
		case <-ctx.Done():
			return fail("auth", "未检测到已登录界面；Cookie 可能失效或页面结构发生变化，未保存访客模式数据")
		case <-time.After(time.Second):
		}
	}
}
