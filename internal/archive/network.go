package archive

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

func proxyURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("代理地址无效；应为 http(s)://host:port 或 socks5://host:port")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("代理仅支持 HTTP、HTTPS、SOCKS5")
	}
	if u.Port() == "" {
		return nil, errors.New("代理地址必须包含端口")
	}
	return u, nil
}

func HTTPClient(rawProxy string, timeout time.Duration) (*http.Client, error) {
	u, err := proxyURL(rawProxy)
	if err != nil {
		return nil, err
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	// Explicit configuration: never accidentally inherit a different environment proxy.
	t.Proxy = nil
	if u != nil {
		t.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: t, Timeout: timeout}, nil
}

// BrowserProxy bridges Chrome's unauthenticated loopback proxy to an authenticated
// HTTP(S) or SOCKS proxy. HTTPS stays end-to-end encrypted; no TLS interception.
type BrowserProxy struct {
	server      *http.Server
	listener    net.Listener
	client      *http.Client
	upstream    *url.URL
	mu          sync.Mutex
	connections map[net.Conn]bool
	closed      bool
}

func StartBrowserProxy(raw string) (*BrowserProxy, error) {
	u, err := proxyURL(raw)
	if err != nil {
		return nil, err
	}
	c, err := HTTPClient(raw, 60*time.Second)
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &BrowserProxy{listener: l, client: c, upstream: u, connections: make(map[net.Conn]bool)}
	b.server = &http.Server{Handler: b, ReadHeaderTimeout: 15 * time.Second}
	go func() { _ = b.server.Serve(l) }()
	return b, nil
}

func (b *BrowserProxy) URL() string { return "http://" + b.listener.Addr().String() }
func (b *BrowserProxy) Close() {
	_ = b.server.Close()
	b.client.CloseIdleConnections()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for c := range b.connections {
		_ = c.Close()
	}
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (b *BrowserProxy) dial(ctx context.Context, address string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	u := b.upstream
	if u == nil {
		return d.DialContext(ctx, "tcp", address)
	}
	if strings.HasPrefix(u.Scheme, "socks5") {
		var auth *proxy.Auth
		if u.User != nil {
			p, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: p}
		}
		s, err := proxy.SOCKS5("tcp", u.Host, auth, d)
		if err != nil {
			return nil, err
		}
		return s.(proxy.ContextDialer).DialContext(ctx, "tcp", address)
	}
	var c net.Conn
	var err error
	if u.Scheme == "https" {
		td := &tls.Dialer{NetDialer: d, Config: &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}}
		c, err = td.DialContext(ctx, "tcp", u.Host)
	} else {
		c, err = d.DialContext(ctx, "tcp", u.Host)
	}
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: make(http.Header)}
	if u.User != nil {
		p, _ := u.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u.User.Username()+":"+p)))
	}
	if err = req.Write(c); err != nil {
		c.Close()
		return nil, err
	}
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, req)
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.StatusCode != 200 {
		c.Close()
		return nil, fmt.Errorf("代理 CONNECT 返回 %d", resp.StatusCode)
	}
	_ = c.SetDeadline(time.Time{})
	return &bufferedConn{Conn: c, reader: r}, nil
}

func (b *BrowserProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		if r.URL.Scheme != "http" {
			http.Error(w, "invalid proxy request", 400)
			return
		}
		out := r.Clone(r.Context())
		out.RequestURI = ""
		out.Header.Del("Proxy-Authorization")
		out.Header.Del("Proxy-Connection")
		resp, err := b.client.Do(out)
		if err != nil {
			http.Error(w, "upstream proxy failed", 502)
			return
		}
		defer resp.Body.Close()
		for k, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	if _, _, err := net.SplitHostPort(r.Host); err != nil {
		http.Error(w, "invalid target", 400)
		return
	}
	up, err := b.dial(r.Context(), r.Host)
	if err != nil {
		http.Error(w, "upstream proxy failed", 502)
		return
	}
	down, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		up.Close()
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		up.Close()
		down.Close()
		return
	}
	b.connections[up] = true
	b.connections[down] = true
	b.mu.Unlock()
	defer func() {
		up.Close()
		down.Close()
		b.mu.Lock()
		delete(b.connections, up)
		delete(b.connections, down)
		b.mu.Unlock()
	}()
	_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	if err = rw.Flush(); err != nil {
		return
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(up, rw); up.Close(); close(done) }()
	_, _ = io.Copy(down, up)
	down.Close()
	up.Close()
	<-done
}

// Never render raw URL errors: they can contain proxy credentials or signed URLs.
func NetworkError(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("请求超时，请检查网络或代理")
	}
	var n net.Error
	if errors.As(err, &n) && n.Timeout() {
		return errors.New("请求超时，请检查网络或代理")
	}
	return errors.New("网络请求失败，请检查代理、DNS、TLS 和连接状态")
}
