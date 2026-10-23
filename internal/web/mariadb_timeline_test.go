package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"xm/internal/archive"
)

func TestMariaDBLargeTimelineUsesOrderedIndexes(t *testing.T) {
	if os.Getenv("XM_TEST_MYSQL_ADDR") == "" {
		t.Skip("requires isolated MariaDB")
	}
	a := testApp(t)
	db := a.store.DB
	if !db.MariaDB {
		t.Skip("MariaDB query-plan regression")
	}
	rows, raw := 30000, "{}"
	if value := os.Getenv("XM_TIMELINE_PERF_ROWS"); value != "" {
		var err error
		rows, err = strconv.Atoi(value)
		if err != nil || rows < 30000 || rows > 1000000 {
			t.Fatal("XM_TIMELINE_PERF_ROWS must be between 30000 and 1000000")
		}
		raw = `{"fixture":"` + strings.Repeat("x", 2048) + `"}`
	}
	// MariaDB's sequence tables generate only this isolated test schema's data.
	_, err := db.Exec(fmt.Sprintf(`INSERT INTO posts(id,author_id,username,text,created_at,url,reply_to,quote_id,repost_id,raw_json,first_seen,last_seen,reply_to_author_id)
 SELECT CAST(90000+seq AS CHAR),'42','alice','fixture','','','','','',?,'','','' FROM seq_1_to_%d`, rows), raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO target_posts(target,post_id) SELECT 'alice',id FROM posts"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("ANALYZE TABLE posts,target_posts"); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"", "alice"} {
		_, where, id, args := timelineSource(db, target, true)
		index, alias := "posts_timeline", "p"
		if target != "" {
			index, alias = "target_posts_timeline", "t"
			where += " AND " + rootPostSQL
		}

		for _, before := range []string{"", "100005", "90050"} {
			for _, span := range timelineRanges(db, id, before) {
				query := "SELECT " + postColumns + timelinePageSource(db, target) + where + span.clause + " ORDER BY " + span.order + " LIMIT 25"
				values := append(append([]any{}, args...), span.args...)
				var plan string
				if err = db.QueryRow("ANALYZE FORMAT=JSON "+query, values...).Scan(&plan); err != nil {
					t.Fatal(err)
				}
				var parsed map[string]any
				if err = json.Unmarshal([]byte(plan), &parsed); err != nil {
					t.Fatal(err)
				}
				var scan map[string]any
				var visit func(any)
				visit = func(v any) {
					switch x := v.(type) {
					case map[string]any:
						if _, ok := x["filesort"]; ok {
							t.Fatalf("unexpected sort target=%q before=%q: %s", target, before, plan)
						}
						if x["table_name"] == alias && x["key"] == index {
							scan = x
						}
						for _, child := range x {
							visit(child)
						}
					case []any:
						for _, child := range x {
							visit(child)
						}
					}
				}
				visit(parsed)
				if scan == nil {
					t.Fatalf("ordered index missing target=%q before=%q: %s", target, before, plan)
				}
				if n, ok := scan["r_rows"].(float64); !ok || n > 25 {
					t.Fatalf("cursor rescanned archive: %s", plan)
				}
			}
			start := time.Now()
			page, err := listPostsPage(context.Background(), db, target, "", "", "posts", 0, 24, before, false)
			if err != nil || len(page.Items) != 24 || !page.HasMore {
				t.Fatalf("page target=%q cursor=%q: %+v %v", target, before, page, err)
			}
			t.Logf("%d rows target=%q cursor=%q page: %s", rows, target, before, time.Since(start))
			// Compare against an independent length-based sort of this single-account
			// fixture. Keep the reference query free of expensive correlated lookups.
			legacy := ""
			values := []any{}
			if before != "" {
				legacy = " AND (length(p.id)<? OR (length(p.id)=? AND p.id<?))"
				values = append(values, len(before), len(before), before)
			}
			start = time.Now()
			expected, err := db.Query("SELECT p.id FROM posts p WHERE 1=1"+legacy+" ORDER BY length(p.id) DESC,p.id DESC LIMIT 25", values...)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for expected.Next() {
				var id string
				if err = expected.Scan(&id); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			err = expected.Err()
			expected.Close()
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%d rows target=%q cursor=%q reference ID sort: %s", rows, target, before, time.Since(start))
			for i, p := range page.Items {
				if p.ID != ids[i] {
					t.Fatalf("ordering changed: %s != %s", p.ID, ids[i])
				}
			}
			if page.NextCursor != ids[23] {
				t.Fatal("cursor changed")
			}
		}
	}
	// Restarting and upgrading an old populated schema must retain all data.
	for _, table := range []string{"posts", "target_posts"} {
		index := "posts_timeline"
		if table == "target_posts" {
			index = "target_posts_timeline"
		}
		if _, err = db.Exec("ALTER TABLE " + table + " DROP INDEX " + index + ", DROP COLUMN xm_id_length"); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a process stopping after the generated column was installed,
	// before its index: retry must finish rather than duplicate the column.
	if _, err = db.Exec("ALTER TABLE posts ADD COLUMN xm_id_length SMALLINT UNSIGNED GENERATED ALWAYS AS (OCTET_LENGTH(id)) VIRTUAL INVISIBLE"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	upgraded, err := archive.OpenConfiguredStore(a.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	t.Logf("upgrade %d rows: %s", rows, time.Since(start))
	reopened, err := archive.OpenConfiguredStore(a.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, table := range []string{"posts", "target_posts"} {
		var n int
		if err = reopened.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != rows {
			t.Fatal("upgrade changed rows", n, err)
		}
		columns, err := reopened.DB.Query("SELECT * FROM " + table + " LIMIT 0")
		if err != nil {
			t.Fatal(err)
		}
		names, err := columns.Columns()
		columns.Close()
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if name == "xm_id_length" {
				t.Fatal("generated column leaked into migration schema")
			}
		}
	}
	// Insert IDs beyond uint64 after upgrading, then traverse a length boundary.
	ids := []string{"9", "99", "100", "184467440737095516151", strings.Repeat("9", 64)}
	var posts []archive.Post
	for _, id := range ids {
		posts = append(posts, archive.Post{ID: id, Username: "edge", Raw: json.RawMessage(`{}`)})
	}
	if err = reopened.SavePosts("edge", posts); err != nil {
		t.Fatal(err)
	}
	var got []string
	cursor := ""
	for {
		page, err := listPostsPage(context.Background(), reopened.DB, "edge", "", "", "", 0, 2, cursor, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range page.Items {
			got = append(got, p.ID)
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	want := []string{ids[4], ids[3], "100", "99", "9"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mixed-length pagination: %v", got)
	}
}
