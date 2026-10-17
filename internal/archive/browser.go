package archive

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,15}$`)

const knownBoundaryPosts = 8

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
	Posts       int
	Pages       int
	Unavailable int
	Stop        string
	Protected   *bool
}
type responseMeta struct {
	operation string
	status    int64
}
type captured struct {
	meta responseMeta
	data []byte
	err  error
}

// BrowserSession owns one Chrome process that can collect several targets in
// sequence. Each target still gets an isolated tab and timeout, while cookies,
// the temporary browser profile and the proxy bridge are shared for the batch.
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
	if err = chromedp.Do(s.root, chromedp.Navigate("about:blank")); err != nil {
		return fail(errors.New("无法启动 Chrome；请检查 chrome_path 和 Chrome 运行环境"))
	}
	return s, nil
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

// Collect reads the browser's actual GraphQL responses. No guest access, query-ID
// guessing, automatic login, or alternate-account fallback is performed.
func Collect(parent context.Context, cfg Config, cookies Cookies, target string, limit int, replies bool, save func(Page) error) (Collection, error) {
	return collect(parent, cfg, cookies, target, limit, replies, save, nil)
}

func CollectBound(parent context.Context, cfg Config, cookies Cookies, target string, identity TargetIdentity, limit int, replies bool, save func(Page) error) (Collection, error) {
	return collectIdentity(parent, cfg, cookies, target, identity, limit, replies, save, nil)
}

// Try the last verified handle first. Only an empty, unsuccessful collection
// may fall back to a stable reference; auth and rate failures are not retried.
func collectIdentity(parent context.Context, cfg Config, cookies Cookies, target string, identity TargetIdentity, limit int, replies bool, save func(Page) error, setup func(context.Context) error) (Collection, error) {
	return collectIdentityWithBrowser(parent, cfg, cookies, target, identity, limit, replies, save, setup, nil)
}

func collectIdentityWithBrowser(parent context.Context, cfg Config, cookies Cookies, target string, identity TargetIdentity, limit int, replies bool, save func(Page) error, setup func(context.Context) error, root context.Context) (Collection, error) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.CollectionTimeout)*time.Second)
	defer cancel()
	saveFailed := false
	checkedSave := func(p Page) error {
		err := save(p)
		if err != nil {
			saveFailed = true
		}
		return err
	}
	result, err := collectBoundWithBrowser(ctx, cfg, cookies, target, identity, limit, replies, checkedSave, setup, root)
	if err == nil || identity.UserID == "" || identity.CurrentUsername == "" || result.Posts > 0 || saveFailed || ctx.Err() != nil || FailureKind(err) == "auth" || FailureKind(err) == "rate" {
		return result, err
	}
	identity.CurrentUsername = ""
	return collectBoundWithBrowser(ctx, cfg, cookies, target, identity, limit, replies, checkedSave, setup, root)
}

func collect(parent context.Context, cfg Config, cookies Cookies, target string, limit int, replies bool, save func(Page) error, setup func(context.Context) error) (Collection, error) {
	return collectBound(parent, cfg, cookies, target, TargetIdentity{}, limit, replies, save, setup)
}
func collectBound(parent context.Context, cfg Config, cookies Cookies, target string, identity TargetIdentity, limit int, replies bool, save func(Page) error, setup func(context.Context) error) (Collection, error) {
	return collectBoundWithBrowser(parent, cfg, cookies, target, identity, limit, replies, save, setup, nil)
}

func collectBoundWithBrowser(parent context.Context, cfg Config, cookies Cookies, target string, identity TargetIdentity, limit int, replies bool, save func(Page) error, setup func(context.Context) error, root context.Context) (Collection, error) {
	var result Collection
	expectedID := identity.UserID
	if expectedID != "" && !userIDPattern.MatchString(expectedID) {
		return result, fail("identity", "无效的绑定用户 ID")
	}
	if limit < 1 || limit > 10000 {
		return result, errors.New("limit 必须在 1～10000 之间")
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.CollectionTimeout)*time.Second)
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
	events := make(chan captured, 128)
	var overflow atomic.Bool
	send := func(c captured) {
		select {
		case events <- c:
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
	responses := chromedp.Events(browser, network.ResponseReceived)
	go func() {
		for e, err := range responses {
			if err != nil {
				return
			}
			op := operation(e.Response.URL)
			if op == "" {
				continue
			}
			meta := responseMeta{operation: op, status: e.Response.Status}
			go func(id network.RequestID, meta responseMeta) {
				if meta.status != 200 {
					send(captured{meta: meta})
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
						send(captured{meta: meta, data: body})
						return
					}
					select {
					case <-readCtx.Done():
						send(captured{meta: meta, err: errors.New("未能读取完整浏览器响应，已入库内容保留，请重试")})
						return
					case <-time.After(250 * time.Millisecond):
					}
				}
			}(e.RequestID, meta)
		}
	}()
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
	scrolls, idle, lastCount, lastPages := 0, 0, 0, 0
	phaseStartPages := 0
	hitScrollLimit := false
	knownStreak := 0
	lastTop, lastHeight := -1.0, -1.0
	ticker := time.NewTicker(time.Duration(cfg.ScrollDelay) * time.Second)
	defer ticker.Stop()
	startOriginalTimeline := func() error {
		replies = false
		pending = nil
		phaseStartPages = result.Pages
		scrolls, idle, knownStreak = 0, 0, 0
		lastTop, lastHeight = -1, -1
		return chromedp.Do(browser, chromedp.Navigate("https://x.com/"+verifiedUsername))
	}
	for {
		if overflow.Load() {
			return result, errors.New("浏览器响应队列溢出，本次采集不完整，请降低采集量")
		}
		select {
		case <-browser.Done():
			return result, errors.New("浏览器已关闭或采集超时；已入库内容保留，可重新采集")
		case c := <-events:
			if c.err != nil {
				return result, c.err
			}
			isDetail := c.meta.operation == "TweetDetail" || c.meta.operation == "TweetResultByRestId"
			if resolvingPost && !isDetail || !resolvingPost && isDetail {
				continue
			}
			switch c.meta.status {
			case 200:
			case 401:
				return result, fail("auth", "Cookie 登录态失效，请重新导入")
			case 403:
				return result, fail("access", "X "+c.meta.operation+" 拒绝访问：可能无查看权限、账号受限或需要浏览器验证")
			case 429:
				return result, fail("rate", "X 请求限流，请等待后重试")
			default:
				return result, fmt.Errorf("X 数据请求返回 HTTP %d", c.meta.status)
			}
			if resolvingPost {
				p, e := IdentityFromPost(c.data, identity.ReferencePostID)
				if e != nil {
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
				return result, e
			}
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
				filtered := []Post{}
				for _, p := range page.Posts {
					// Author ID is authoritative. Historical posts may still
					// carry an old handle; quoted authors remain separate.
					if p.AuthorID != verifiedID || seen[p.ID] || len(seen) >= limit {
						continue
					}
					if len(identity.KnownPostIDs) > 0 {
						if identity.KnownPostIDs[p.ID] {
							knownStreak++
						} else {
							knownStreak = 0
						}
					}
					seen[p.ID] = true
					filtered = append(filtered, p)
				}
				page.Posts = filtered
				if err = save(page); err != nil {
					return result, err
				}
				result.Posts += len(filtered)
				result.Unavailable += page.Unavailable
				if page.Timeline {
					result.Pages++
				}
				if result.Posts >= limit {
					result.Stop = "limit_reached"
					return result, nil
				}
			}
			// A run still starts at the top so new posts cannot be skipped. Once
			// eight consecutive timeline entries are already archived, older pages
			// are outside the incremental window and need not be fetched again.
			if len(identity.KnownPostIDs) > 0 && knownStreak >= knownBoundaryPosts {
				if replies {
					if err = startOriginalTimeline(); err != nil {
						return result, errors.New("回复增量边界已到达，但无法打开普通推文时间线")
					}
					continue
				}
				result.Stop = "known_boundary_reached"
				return result, nil
			}
		case <-ticker.C:
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
			// is not exhaustion. Only count idle checks at a stable bottom, after
			// both scrolling and timeline responses have stopped making progress.
			if state.Bottom && state.Top == lastTop && state.Height == lastHeight && result.Posts == lastCount && result.Pages == lastPages {
				idle++
			} else {
				idle = 0
			}
			lastCount = result.Posts
			lastPages = result.Pages
			lastTop, lastHeight = state.Top, state.Height
			if scrolls >= cfg.MaxScrolls || idle >= 8 {
				if verifiedID == "" {
					return result, fail("identity", "未能确认目标用户 ID，未保存推文")
				}
				if result.Pages == phaseStartPages {
					return result, errors.New("未收到可读取的用户时间线，不能认定为没有推文；请检查登录态、权限、代理或页面变化")
				}
				if result.Posts == 0 && result.Unavailable > 0 {
					return result, errors.New("时间线仅返回不可访问的内容，无法归档")
				}
				if scrolls >= cfg.MaxScrolls {
					hitScrollLimit = true
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
				if hitScrollLimit {
					result.Stop = "max_scrolls_reached"
				}
				return result, nil
			}
			// Reach the loaded timeline's end to trigger its next page. Small fixed
			// scrolls can consume the entire budget inside a single media-heavy page.
			_, err = chromedp.Run(browser, chromedp.Evaluate[any](`(()=>{const s=document.scrollingElement||document.documentElement;s.scrollTo({top:s.scrollHeight,behavior:'instant'})})()`))
			if err != nil {
				return result, errors.New("页面滚动失败")
			}
			scrolls++
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
