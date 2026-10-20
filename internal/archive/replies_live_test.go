package archive

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"xm/internal/database"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// Explicitly opt-in: inspect operation names/status only, discard captured posts.
func TestLiveReplies(t *testing.T) {
	path := os.Getenv("XM_LIVE_REPLIES_CONFIG")
	if path == "" {
		t.Skip("set XM_LIVE_REPLIES_CONFIG to opt into an authenticated X read")
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	filename, err := cfg.Database.SQLiteFilename(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(cfg.DataDir, filename)}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cookies, err := (&Store{DB: &database.DB{DB: db, Dialect: "sqlite"}}).LoadCookies()
	if err != nil {
		t.Fatal("Cookie unavailable")
	}
	var target string
	if err = db.QueryRow("SELECT target FROM runs WHERE status='failed' ORDER BY id DESC LIMIT 1").Scan(&target); err != nil {
		t.Fatal(err)
	}
	identity := TargetIdentity{CurrentUsername: target}
	err = db.QueryRow("SELECT user_id,current_username FROM target_identities WHERE target=?", target).Scan(&identity.UserID, &identity.CurrentUsername)
	if err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	cfg.BrowserMaxAge = 60
	cfg.ScrollDelay = 1
	cfg.MaxScrolls = 5
	var mu sync.Mutex
	ops := map[string]int64{}
	setup := func(ctx context.Context) error {
		events := chromedp.Events(ctx, network.ResponseReceived)
		go func() {
			for e, err := range events {
				if err != nil {
					return
				}
				u, err := url.Parse(e.Response.URL)
				if err != nil || u.Hostname() != "x.com" || !strings.HasPrefix(u.Path, "/i/api/graphql/") {
					continue
				}
				parts := strings.Split(u.Path, "/")
				mu.Lock()
				ops[parts[len(parts)-1]] = e.Response.Status
				mu.Unlock()
			}
		}()
		return nil
	}
	count, replies := 0, 0
	result, err := collectBound(context.Background(), cfg, cookies, target, identity, 3, true, func(p Page) error {
		for _, post := range p.Posts {
			count++
			if post.ReplyTo != "" {
				replies++
			}
		}
		return nil
	}, setup)
	mu.Lock()
	keys := []string{}
	for op := range ops {
		keys = append(keys, op)
	}
	sort.Strings(keys)
	for _, op := range keys {
		t.Logf("operation=%s status=%d", op, ops[op])
	}
	mu.Unlock()
	t.Logf("posts=%d replies=%d timeline_pages=%d", count, replies, result.Pages)
	if err != nil {
		t.Fatal(err)
	}
}
