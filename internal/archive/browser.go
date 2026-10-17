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

func operation(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() != "x.com" || !strings.HasPrefix(u.Path, "/i/api/graphql/") {
		return ""
	}
	parts := strings.Split(u.Path, "/")
	s := parts[len(parts)-1]
	switch s {
	case "UserByScreenName", "UserTweets", "UserTweetsAndReplies":
		return s
	}
	return ""
}

// Collect reads the browser's actual GraphQL responses. No guest access, query-ID
// guessing, automatic login, or alternate-account fallback is performed.
func Collect(parent context.Context, cfg Config, cookies Cookies, target string, limit int, replies bool, save func(Page) error) (Collection, error) {
	return collect(parent, cfg, cookies, target, limit, replies, save, nil)
}

func collect(parent context.Context, cfg Config, cookies Cookies, target string, limit int, replies bool, save func(Page) error, setup func(context.Context) error) (Collection, error) {
	var result Collection
	if limit < 1 || limit > 10000 {
		return result, errors.New("limit 必须在 1～10000 之间")
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.CollectionTimeout)*time.Second)
	defer cancel()
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
	browser, closeBrowser := chromedp.NewContext(alloc, chromedp.WithErrorf(func(string, ...any) {}))
	defer closeBrowser()
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
	if replies {
		path += "/with_replies"
	}
	if err = chromedp.Do(browser, chromedp.Navigate(path)); err != nil {
		return result, errors.New("无法打开 X 主页，请检查代理和网络；也可能已达到采集超时")
	}
	if err = confirmSession(browser); err != nil {
		return result, err
	}
	seen := make(map[string]bool)
	scrolls, idle, lastCount := 0, 0, 0
	ticker := time.NewTicker(time.Duration(cfg.ScrollDelay) * time.Second)
	defer ticker.Stop()
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
			switch c.meta.status {
			case 200:
			case 401:
				return result, fail("auth", "Cookie 登录态失效，请重新导入")
			case 403:
				return result, fail("access", "X 拒绝访问：可能无查看权限、账号受限或需要浏览器验证")
			case 429:
				return result, fail("rate", "X 请求限流，请等待后重试")
			default:
				return result, fmt.Errorf("X 数据请求返回 HTTP %d", c.meta.status)
			}
			page, e := ParsePage(c.data, c.meta.operation)
			if e != nil {
				return result, e
			}
			if page.Profile != nil {
				if !strings.EqualFold(page.Profile.Username, target) {
					continue
				}
				protected := page.Profile.Protected
				result.Protected = &protected
			}
			filtered := []Post{}
			for _, p := range page.Posts {
				// Conversation modules may also contain posts by other authors.
				if !strings.EqualFold(p.Username, target) || seen[p.ID] || len(seen) >= limit {
					continue
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
		case <-ticker.C:
			type pageState struct {
				Path   string `json:"path"`
				Denied bool   `json:"denied"`
			}
			state, e := chromedp.Run(browser, chromedp.Evaluate[pageState](`({path:location.pathname,denied:/These posts are protected|These Tweets are protected|Only approved followers|这些帖子受到保护|这些推文受到保护/.test(document.body.innerText)})`))
			if e != nil {
				return result, errors.New("无法读取浏览器状态，浏览器可能已关闭")
			}
			if strings.HasPrefix(state.Path, "/i/flow/login") || strings.HasPrefix(state.Path, "/account/access") {
				return result, fail("auth", "X 要求重新登录或人工验证，请在浏览器处理后重新导入 Cookie")
			}
			if state.Denied {
				return result, fail("access", "当前账号无权查看此锁推账号，请确认关注请求已经获准")
			}
			if result.Posts == lastCount {
				idle++
			} else {
				idle = 0
			}
			lastCount = result.Posts
			if scrolls >= cfg.MaxScrolls || idle >= 8 {
				if result.Pages == 0 {
					return result, errors.New("未收到可读取的用户时间线，不能认定为没有推文；请检查登录态、权限、代理或页面变化")
				}
				if result.Posts == 0 && result.Unavailable > 0 {
					return result, errors.New("时间线仅返回不可访问的内容，无法归档")
				}
				result.Stop = "no_new_posts_observed"
				if scrolls >= cfg.MaxScrolls {
					result.Stop = "max_scrolls_reached"
				}
				return result, nil
			}
			_, err = chromedp.Run(browser, chromedp.Evaluate[any](`window.scrollBy(0, Math.max(600, window.innerHeight * 0.8))`))
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
