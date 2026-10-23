package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

func ValidateBrowser(c Config) error {
	if c.BrowserMode != "" && c.BrowserMode != "local" && c.BrowserMode != "cdp" {
		return errors.New("浏览器模式必须为 local 或 cdp")
	}
	if c.CDPHost != "" && c.CDPHost != "localhost" && net.ParseIP(c.CDPHost) == nil {
		return errors.New("CDP 地址请填写 IPv4、IPv6 或 localhost，不包含协议、路径和端口")
	}
	if c.CDPPort < 0 || c.CDPPort > 65535 || (c.BrowserMode == "cdp" && (c.CDPHost == "" || c.CDPPort == 0)) {
		return errors.New("CDP 模式需要填写 IP 和 1～65535 范围内的端口")
	}
	return nil
}

// Resolve discovery ourselves: malformed service/management-page responses
// must be errors, and a container's advertised loopback address must not
// replace the configured reachable endpoint. CDP does not use the X proxy.
func browserWebSocket(ctx context.Context, cfg Config) (string, error) {
	if err := ValidateBrowser(cfg); err != nil {
		return "", err
	}
	host := net.JoinHostPort(cfg.CDPHost, strconv.Itoa(cfg.CDPPort))
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+host+"/json/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", chromeCheckCause("chrome", fmt.Sprintf("CDP %s /json/version 请求失败：%s", host, browserConnectionReason(err)), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("CDP /json/version 返回 HTTP %d（%s）；请确认 Chrome 调试端口、访问权限及端口映射", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	var info struct {
		WebSocket string `json:"webSocketDebuggerUrl"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		var networkErr net.Error
		if errors.As(err, &networkErr) || ctx.Err() != nil {
			return "", chromeCheckCause("chrome", "读取 CDP /json/version 响应失败："+browserConnectionReason(err), err)
		}
		return "", errors.New("CDP 返回无效响应，请填写 Chrome 调试端口而非管理页面端口")
	}
	u, err := url.Parse(info.WebSocket)
	if err != nil || u.Scheme != "ws" || u.Host == "" || u.User != nil || !strings.HasPrefix(u.Path, "/devtools/browser/") || len(strings.TrimPrefix(u.Path, "/devtools/browser/")) == 0 {
		return "", errors.New("CDP 响应缺少有效的浏览器 WebSocket 地址")
	}
	u.Host = host
	return u.String(), nil
}

// Report actionable causes without echoing a remote response body, debugger
// URL (which may carry tokens), or local Chrome process output.
func browserConnectionReason(err error) string {
	var networkErr net.Error
	var protocolErr *cdproto.Error
	switch {
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "对端提前关闭连接，请检查 Chrome 服务和端口映射"
	case errors.Is(err, context.Canceled):
		return "测试已取消"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &networkErr) && networkErr.Timeout():
		return "连接或响应超时，请检查地址、防火墙及浏览器是否响应"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "连接被拒绝，请确认 Chrome 已启用远程调试并监听该地址和端口"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return "网络或主机不可达，请检查 IP、路由及防火墙"
	case errors.As(err, &protocolErr):
		return fmt.Sprintf("CDP 协议错误（%d）；请确认远端支持创建独立浏览器上下文", protocolErr.Code)
	}
	if strings.Contains(err.Error(), "websocket handshake failed") {
		if _, status, found := strings.Cut(err.Error(), `unexpected status "`); found {
			var code int
			if _, e := fmt.Sscanf(status, "%d", &code); e == nil && code >= 100 && code <= 599 {
				return fmt.Sprintf("WebSocket 握手被拒绝（HTTP %d）；请检查调试端口权限及反向代理的 WebSocket 支持", code)
			}
		}
		return "WebSocket 握手响应无效，请检查调试端口及反向代理的 WebSocket 支持"
	}
	return "连接建立或 CDP 会话初始化失败，请检查调试服务及独立浏览器上下文支持"
}

// One owner for allocation and cleanup across diagnostics, CLI collection and
// scheduler reuse. Remote sessions own an isolated browser context, never the
// external process or its existing tabs/cookies. Do not fall back to local.
func newBrowserContext(parent context.Context, cfg Config, useProxy bool) (context.Context, context.CancelFunc, error) {
	if err := ValidateBrowser(cfg); err != nil {
		return nil, nil, err
	}
	if cfg.BrowserMode == "cdp" {
		ws, err := browserWebSocket(parent, cfg)
		if err != nil {
			return nil, nil, err
		}
		alloc, closeAlloc := remote.NewAllocator(parent, ws, remote.NoModifyURL)
		ctx, closeBrowser := chromedp.NewContext(alloc, chromedp.WithNewBrowserContext(), chromedp.WithErrorf(func(string, ...any) {}))
		return ctx, sync.OnceFunc(func() {
			closeBrowser()
			closeAlloc()
			// Unlike NewExecAllocator, remote.NewAllocator's cancel function
			// does not wait. Join its cleanup before releasing session ownership.
			chromedp.FromContext(alloc).Allocator.Wait()
		}), nil
	}
	// Let ExecAllocator own its temporary profile. Passing UserDataDir marks
	// it as caller-owned and disables child-process cleanup and removal retries.
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.Flag("headless", cfg.Headless), chromedp.Flag("disable-dev-shm-usage", true), chromedp.Flag("disable-quic", true))
	if cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(cfg.ChromePath))
	}
	var bridge *BrowserProxy
	if useProxy && cfg.Proxy != "" {
		var err error
		bridge, err = StartBrowserProxy(cfg.Proxy)
		if err != nil {
			return nil, nil, chromeCheckError("network", "无法初始化采集代理："+err.Error())
		}
		opts = append(opts, chromedp.ProxyServer(bridge.URL()))
	} else {
		opts = append(opts, chromedp.Flag("no-proxy-server", true))
	}
	alloc, closeAlloc := chromedp.NewExecAllocator(parent, opts...)
	ctx, closeBrowser := chromedp.NewContext(alloc, chromedp.WithErrorf(func(string, ...any) {}))
	return ctx, sync.OnceFunc(func() {
		closeBrowser()
		closeAlloc()
		if bridge != nil {
			bridge.Close()
		}
	}), nil
}

func browserStartError(cfg Config) string {
	if cfg.BrowserMode == "cdp" {
		return "无法连接 CDP 浏览器或创建独立会话；请检查调试端口及浏览器是否支持独立浏览器上下文"
	}
	return "无法启动 Chrome；请检查 chrome_path 和 Chrome 运行环境"
}
