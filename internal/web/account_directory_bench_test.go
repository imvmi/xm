package web

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"xm/internal/archive"
)

func BenchmarkAccountDirectory(b *testing.B) {
	for _, size := range []int{100000, 500000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			s, err := archive.OpenStore(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { s.Close() })
			if err := migrate(s); err != nil {
				b.Fatal(err)
			}
			for _, q := range []string{
				fmt.Sprintf(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<%d) INSERT INTO posts(id,author_id,username,text,created_at,url,reply_to,quote_id,repost_id,raw_json,first_seen,last_seen) SELECT CAST(x AS TEXT),'42','bob','','','','','','','','','' FROM n`, size),
				"INSERT INTO target_posts SELECT 'bob',id FROM posts",
			} {
				if _, err := s.DB.Exec(q); err != nil {
					b.Fatal(err)
				}
			}
			// Use the normal write path once for each account as well.
			for _, target := range []string{"alice", "bob"} {
				if err := s.SavePosts(target, []archive.Post{{ID: target, Username: target}}); err != nil {
					b.Fatal(err)
				}
			}
			a := &App{store: s}
			b.Run("directory", func(b *testing.B) {
				for b.Loop() {
					if _, err := archiveUserDirectory(s.DB, ""); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("api_accounts", func(b *testing.B) {
				for b.Loop() {
					if _, err := a.externalAccountRows(context.Background(), apiCredential{AllAccounts: true}, "", "", 50); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("one_account_stats", func(b *testing.B) {
				for b.Loop() {
					w := httptest.NewRecorder()
					a.userStats(w, httptest.NewRequest("GET", "/api/users/stats?target=alice&refresh=1", nil))
					if w.Code != 200 {
						b.Fatal(w.Body.String())
					}
				}
			})
		})
	}
}
