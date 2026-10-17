package archive

import (
	"context"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthenticatedHTTPProxyForBrowserAndMedia(t *testing.T) {
	var destinationLeak atomic.Bool
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			destinationLeak.Store(true)
		}
		w.Write([]byte("proxied"))
	}))
	defer target.Close()
	forward, err := StartBrowserProxy("")
	if err != nil {
		t.Fatal(err)
	}
	defer forward.Close()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" {
			http.Error(w, "missing proxy auth", 407)
			return
		}
		calls.Add(1)
		forward.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	raw := strings.Replace(upstream.URL, "http://", "http://user:pass@", 1)
	bridge, err := StartBrowserProxy(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	for _, p := range []string{bridge.URL(), raw} {
		c, err := HTTPClient(p, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.CloseIdleConnections()
		tr := c.Transport.(*http.Transport)
		tr.TLSClientConfig.RootCAs = roots
		resp, err := c.Get(target.URL)
		if err != nil {
			t.Fatal(NetworkError(err))
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "proxied" {
			t.Fatal("proxy did not tunnel")
		}
	}
	if calls.Load() != 2 || destinationLeak.Load() {
		t.Fatal("wrong routing or credentials leaked", calls.Load())
	}
}

func TestAuthenticatedSOCKS5RemoteDNS(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("socks-ok")) }))
	defer target.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	var calls atomic.Int32
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				header := make([]byte, 2)
				if _, e := io.ReadFull(c, header); e != nil {
					return
				}
				methods := make([]byte, int(header[1]))
				if _, e := io.ReadFull(c, methods); e != nil {
					return
				}
				c.Write([]byte{5, 2}) // Require username/password.
				if _, e := io.ReadFull(c, header); e != nil {
					return
				}
				user := make([]byte, int(header[1]))
				if _, e := io.ReadFull(c, user); e != nil {
					return
				}
				length := make([]byte, 1)
				if _, e := io.ReadFull(c, length); e != nil {
					return
				}
				password := make([]byte, int(length[0]))
				if _, e := io.ReadFull(c, password); e != nil {
					return
				}
				if string(user) != "user" || string(password) != "pass" {
					c.Write([]byte{1, 1})
					return
				}
				c.Write([]byte{1, 0})
				request := make([]byte, 4)
				if _, e := io.ReadFull(c, request); e != nil {
					return
				}
				if request[0] != 5 || request[1] != 1 || request[3] != 3 {
					return
				} // Domain name, not locally resolved IP.
				if _, e := io.ReadFull(c, length); e != nil {
					return
				}
				host := make([]byte, int(length[0]))
				if _, e := io.ReadFull(c, host); e != nil {
					return
				}
				port := make([]byte, 2)
				if _, e := io.ReadFull(c, port); e != nil {
					return
				}
				if string(host) != "example.com" {
					return
				}
				up, e := net.Dial("tcp", target.Listener.Addr().String())
				if e != nil {
					return
				}
				defer up.Close()
				c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
				calls.Add(1)
				done := make(chan struct{})
				go func() { io.Copy(up, c); up.Close(); close(done) }()
				io.Copy(c, up)
				c.Close()
				<-done
			}(c)
		}
	}()
	raw := "socks5://user:pass@" + l.Addr().String()
	bridge, err := StartBrowserProxy(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	roots := x509.NewCertPool()
	roots.AddCert(target.Certificate())
	_, port, _ := net.SplitHostPort(target.Listener.Addr().String())
	for _, p := range []string{bridge.URL(), raw} {
		c, e := HTTPClient(p, 5*time.Second)
		if e != nil {
			t.Fatal(e)
		}
		defer c.CloseIdleConnections()
		c.Transport.(*http.Transport).TLSClientConfig.RootCAs = roots
		resp, e := c.Get("https://example.com:" + port)
		if e != nil {
			t.Fatal(e)
		}
		body, e := io.ReadAll(resp.Body)
		resp.Body.Close()
		if e != nil || string(body) != "socks-ok" {
			t.Fatal(string(body), e)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("SOCKS routing failed", calls.Load())
	}
}

func TestProxyConfigAndErrorRedaction(t *testing.T) {
	for _, raw := range []string{"http://localhost:7890", "https://u:p@localhost:7890", "socks5://u:p@localhost:1080", "socks5h://localhost:1080", ""} {
		if _, err := proxyURL(raw); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"ftp://localhost:123", "localhost:123", "http://localhost", "http://u:secret@bad:port"} {
		if _, err := proxyURL(raw); err == nil {
			t.Fatal("invalid proxy accepted", raw)
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatal("proxy secret leaked")
		}
	}
	if NetworkError(context.DeadlineExceeded) == nil {
		t.Fatal("missing timeout error")
	}
}
