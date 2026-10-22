package archive

import (
	"context"
	"errors"
	"net/url"
	"os"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

type ChromeInfo struct {
	Product  string
	Headless bool
}

type ChromeCheckError struct {
	Stage   string
	Message string
}

func (e *ChromeCheckError) Error() string { return e.Message }

func ChromeCheckStage(err error) string {
	if err == nil {
		return ""
	}
	var e *ChromeCheckError
	if errors.As(err, &e) {
		return e.Stage
	}
	return "chrome"
}

func chromeCheckError(stage, message string) error {
	return &ChromeCheckError{Stage: stage, Message: message}
}

// CheckChrome starts Chrome with the same local runtime options used by the
// collector. When cookies is non-nil it also opens X and verifies the session,
// but never reads or stores timeline data.
func CheckChrome(parent context.Context, cfg Config, cookies *Cookies) (ChromeInfo, error) {
	var info ChromeInfo
	profile, err := os.MkdirTemp("", "xm-chrome-check-*")
	if err != nil {
		return info, chromeCheckError("chrome", "无法创建 Chrome 临时目录："+err.Error())
	}
	defer os.RemoveAll(profile)

	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.UserDataDir(profile),
		chromedp.Flag("headless", cfg.Headless),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-quic", true),
	)
	var bridge *BrowserProxy
	if cookies != nil && cfg.Proxy != "" {
		bridge, err = StartBrowserProxy(cfg.Proxy)
		if err != nil {
			return info, chromeCheckError("network", "无法初始化采集代理："+err.Error())
		}
		defer bridge.Close()
		opts = append(opts, chromedp.ProxyServer(bridge.URL()))
	} else {
		opts = append(opts, chromedp.Flag("no-proxy-server", true))
	}
	if cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(cfg.ChromePath))
	}
	alloc, closeAlloc := chromedp.NewExecAllocator(parent, opts...)
	defer closeAlloc()
	ctx, closeBrowser := chromedp.NewContext(alloc, chromedp.WithErrorf(func(string, ...any) {}))
	defer closeBrowser()

	info.Product, err = chromedp.Run(ctx, chromedp.Evaluate[string](`navigator.userAgent.match(/(?:HeadlessChrome|Chrome|Chromium)\/[0-9.]+/)?.[0]||navigator.userAgent`))
	if err != nil {
		return ChromeInfo{}, chromeCheckError("chrome", "无法启动 Chrome："+err.Error()+"。请确认已安装 Chrome/Chromium，并检查 chrome_path、运行权限和沙箱环境")
	}
	info.Headless = cfg.Headless
	if cookies == nil {
		return info, nil
	}
	if err = chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
		for _, v := range []struct{ name, value string }{{"auth_token", cookies.AuthToken}, {"ct0", cookies.CSRF}} {
			secure, httponly := true, v.name == "auth_token"
			if _, e := cdp.Call(c, target, network.SetCookie, network.SetCookieParams{Name: v.name, Value: v.value, Domain: ".x.com", Path: "/", Secure: &secure, HTTPOnly: &httponly}); e != nil {
				return e
			}
		}
		return nil
	})); err != nil {
		return info, chromeCheckError("cookie", "Chrome 无法导入 Cookie；请重新导入 Cookie 后再试")
	}
	if err = navigateChromeDocument(ctx, "https://x.com/home"); err != nil {
		reason := err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "等待主页面超时"
		}
		return info, chromeCheckError("network", "Chrome 未能载入 X 主页面："+reason+"。HTTP 连通性测试仅访问 robots.txt；请检查代理对 x.com 及页面资源的连接情况")
	}
	if err = confirmSession(ctx); err != nil {
		return info, chromeCheckError("session", err.Error())
	}
	return info, nil
}

// A diagnostic needs the main document and login UI, not the window load event.
// Slow images must not turn a usable logged-in page into a network failure.
func navigateChromeDocument(ctx context.Context, targetURL string) error {
	expected, err := url.Parse(targetURL)
	if err != nil {
		return err
	}
	if err = chromedp.Do(ctx, chromedp.Func(func(c context.Context, target *chromedp.Target) error {
		result, err := cdp.Call(c, target, page.Navigate, page.NavigateParams{URL: targetURL})
		if err != nil {
			return err
		}
		if result.ErrorText != "" {
			return errors.New(result.ErrorText)
		}
		return nil
	})); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, evalErr := chromedp.Run(ctx, chromedp.Evaluate[struct {
			URL   string `json:"url"`
			Ready string `json:"ready"`
		}](`({url:location.href,ready:document.readyState})`))
		if evalErr == nil {
			actual, parseErr := url.Parse(state.URL)
			if parseErr == nil && actual.Scheme == expected.Scheme && actual.Host == expected.Host && (state.Ready == "interactive" || state.Ready == "complete") {
				return nil
			}
		}
		// Redirects can temporarily destroy the execution context; retry until the
		// destination document is usable. Never accept about:blank or an error page.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
