package archive

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/remote"
)

func TestCDPBrowserIsolationAndCleanupOffline(t *testing.T) {
	if os.Getenv("XM_BROWSER_TEST") != "1" {
		t.Skip("real Chrome CDP fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	profile := t.TempDir()
	// Chrome children can briefly finish writing after the main process exits.
	t.Cleanup(func() {
		for range 50 {
			if err := os.RemoveAll(profile); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Error("external Chrome fixture profile was not released")
	})
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.UserDataDir(profile), remote.WebSocket)
	alloc, closeAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer closeAlloc()
	owner, closeOwner := chromedp.NewContext(alloc)
	defer closeOwner()
	var err error
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>fixture</html>")) }))
	defer page.Close()
	if err = chromedp.Do(owner, chromedp.Navigate(page.URL)); err != nil {
		t.Fatal(err)
	}
	if _, err = chromedp.Run(owner, chromedp.Evaluate[string](`document.cookie='owner=keep';document.cookie`)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
	if err != nil {
		t.Fatal(err)
	}
	port := strings.Split(string(data), "\n")[0]
	cfg := cdpFixtureConfig(t, "http://127.0.0.1:"+port)
	// Invalid local-only options must not be used for a remote connection.
	cfg.ChromePath = "/does-not-exist/chrome"
	cfg.Proxy = "http://127.0.0.1:1"
	session, err := NewBrowserSession(ctx, cfg, Cookies{AuthToken: "synthetic-cdp", CSRF: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err = chromedp.Do(session.root, chromedp.Navigate(page.URL)); err != nil {
		t.Fatal(err)
	}
	cookies, err := chromedp.Run(session.root, chromedp.Evaluate[string](`document.cookie`))
	if err != nil || cookies != "" {
		t.Fatal("remote session reused existing cookies", cookies, err)
	}
	owned := chromedp.FromContext(session.root).BrowserContextID
	if owned == "" {
		t.Fatal("remote session was not isolated")
	}
	stop := errors.New("stop before X network access")
	_, err = session.collectBound(ctx, "alice", TargetIdentity{}, 10, false, func(Page) error { return nil }, func(browser context.Context) error {
		err := chromedp.Do(browser, chromedp.Func(func(c context.Context, tgt *chromedp.Target) error {
			got, e := cdp.Call(c, tgt, network.GetCookies, network.GetCookiesParams{URLs: []string{"https://x.com/"}})
			if e != nil {
				return e
			}
			for _, cookie := range got.Cookies {
				if cookie.Name == "auth_token" && cookie.Value == "synthetic-cdp" {
					return nil
				}
			}
			return errors.New("collector did not import saved cookies into remote context")
		}))
		if err != nil {
			t.Error(err)
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal("CDP collector did not reach setup", err)
	}
	session.Close()
	if err = chromedp.Do(owner, chromedp.Func(func(c context.Context, _ *chromedp.Target) error {
		contexts, e := cdp.Call(c, chromedp.FromContext(owner).Browser, target.GetBrowserContexts, cdp.Empty{})
		if e != nil {
			return e
		}
		for _, id := range contexts.BrowserContextIDs {
			if id == owned {
				t.Error("XM context leaked")
			}
		}
		return nil
	})); err != nil {
		t.Fatal("closing session closed remote browser", err)
	}
	cookies, err = chromedp.Run(owner, chromedp.Evaluate[string](`document.cookie`))
	if err != nil || cookies != "owner=keep" {
		t.Fatal("existing page/cookies changed", cookies, err)
	}
	info, err := CheckChrome(ctx, cfg, nil)
	if err != nil || info.Product == "" {
		t.Fatal("CDP diagnostic failed", info, err)
	}
	if _, err = chromedp.Run(owner, chromedp.Evaluate[bool](`true`)); err != nil {
		t.Fatal("diagnostic closed remote browser", err)
	}
	// A canceled remote session must release its isolated context
	// while preserving the owner's browser. Switching to local must own a
	// separate process/profile, and switching back must still be possible.
	for _, mode := range []string{"cdp", "local", "cdp"} {
		child, cancelChild := context.WithCancel(ctx)
		current := cfg
		current.BrowserMode = mode
		if mode == "local" {
			current.ChromePath = ""
			current.Proxy = ""
		}
		s, err := NewBrowserSession(child, current, Cookies{})
		if err != nil {
			cancelChild()
			t.Fatal(err)
		}
		profile := ""
		id := chromedp.FromContext(s.root).BrowserContextID
		if mode == "local" {
			profile = localBrowserProfile(t, s.root)
		}
		cancelChild()
		s.Close()
		s.Close()
		if profile != "" {
			if _, err := os.Stat(profile); !os.IsNotExist(err) {
				t.Fatal("local profile survived cancellation", err)
			}
		}
		if err = chromedp.Do(owner, chromedp.Func(func(c context.Context, _ *chromedp.Target) error {
			contexts, err := cdp.Call(c, chromedp.FromContext(owner).Browser, target.GetBrowserContexts, cdp.Empty{})
			if err != nil {
				return err
			}
			for _, existing := range contexts.BrowserContextIDs {
				if existing == id {
					t.Error("canceled remote context leaked")
				}
			}
			return nil
		})); err != nil {
			t.Fatal("mode switch closed owner browser", err)
		}
	}
}
