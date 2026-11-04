package web

import (
	"fmt"
	"testing"

	"xm/internal/archive"
)

// Run with: go test ./internal/web -run '^$' -bench BenchmarkArchiveHotpaths -benchtime=3x
func BenchmarkArchiveHotpaths(b *testing.B) {
	for _, size := range []int{10000, 100000, 500000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			s, err := archive.OpenStore(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { s.Close() })
			if err := migrate(s); err != nil {
				b.Fatal(err)
			}
			a := &App{store: s}
			a.notifier = newNotifier(a)
			for _, query := range []string{
				fmt.Sprintf(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<%d) INSERT INTO posts(id,author_id,username,text,created_at,url,reply_to,quote_id,repost_id,raw_json,first_seen,last_seen) SELECT CAST(x AS TEXT),'42','alice','','','','','','','','','' FROM n`, size),
				"INSERT INTO target_posts SELECT 'alice',id FROM posts",
				"INSERT INTO monitors(username,enabled,next_run) VALUES('alice',1,0)",
				"INSERT INTO run_metrics(run_id,new_posts,posts_read) VALUES(1,0,0)",
			} {
				if _, err := s.DB.Exec(query); err != nil {
					b.Fatal(err)
				}
			}
			m := Monitor{Username: "alice", LastSuccess: 1}
			if err := a.prepareNotifications(m); err != nil {
				b.Fatal(err)
			}
			b.Run("scheduler_read", func(b *testing.B) {
				for b.Loop() {
					if _, err := scanMonitor(s.DB.QueryRow("SELECT " + monitorRuntimeColumns + " FROM monitors m WHERE username='alice'")); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("notifications_unchanged", func(b *testing.B) {
				for b.Loop() {
					if err := a.recordNotifications(archive.Config{}, m, true, false, "", 0); err != nil {
						b.Fatal(err)
					}
				}
			})
			nextID := size
			b.Run("notifications_10_new", func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					posts := make([]archive.Post, 10)
					for j := range posts {
						nextID++
						posts[j] = archive.Post{ID: fmt.Sprint(nextID), Username: "alice"}
					}
					if err := s.SavePosts("alice", posts); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
					if err := a.recordNotifications(archive.Config{}, m, true, false, "", 0); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
