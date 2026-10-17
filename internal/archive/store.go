package archive

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ DB *sql.DB }

func OpenStore(dir string) (*Store, error) {
	if err := PrivateDir(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "archive.sqlite")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;
CREATE TABLE IF NOT EXISTS posts (
 id TEXT PRIMARY KEY, author_id TEXT NOT NULL, username TEXT NOT NULL, text TEXT NOT NULL,
 created_at TEXT NOT NULL, url TEXT NOT NULL, reply_to TEXT NOT NULL, quote_id TEXT NOT NULL,
 repost_id TEXT NOT NULL, raw_json TEXT NOT NULL, first_seen TEXT NOT NULL, last_seen TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS target_posts (
 target TEXT NOT NULL, post_id TEXT NOT NULL REFERENCES posts(id), PRIMARY KEY(target,post_id)
);
CREATE TABLE IF NOT EXISTS profiles (
 id TEXT PRIMARY KEY, username TEXT NOT NULL, protected INTEGER NOT NULL, checked_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS media (
 post_id TEXT NOT NULL REFERENCES posts(id), media_id TEXT NOT NULL, kind TEXT NOT NULL,
 source_url TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', local_path TEXT NOT NULL DEFAULT '',
 bytes INTEGER NOT NULL DEFAULT 0, sha256 TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL,
 PRIMARY KEY(post_id,media_id)
);
CREATE INDEX IF NOT EXISTS media_status ON media(status);
CREATE TABLE IF NOT EXISTS runs (
 id INTEGER PRIMARY KEY, target TEXT NOT NULL, started_at TEXT NOT NULL, finished_at TEXT,
 status TEXT NOT NULL, posts_seen INTEGER NOT NULL DEFAULT 0, message TEXT NOT NULL DEFAULT ''
);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = migrateStorage(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db}, nil
}

func now() string             { return time.Now().UTC().Format(time.RFC3339Nano) }
func (s *Store) Close() error { return s.DB.Close() }
func (s *Store) StartRun(target string) (int64, error) {
	r, err := s.DB.Exec("INSERT INTO runs(target,started_at,status) VALUES(?,?,'running')", target, now())
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}
func (s *Store) EndRun(id int64, status string, count int, message string) error {
	_, err := s.DB.Exec("UPDATE runs SET finished_at=?,status=?,posts_seen=?,message=? WHERE id=?", now(), status, count, message, id)
	return err
}
func (s *Store) SaveProfile(p Profile) error {
	_, err := s.DB.Exec(`INSERT INTO profiles VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET username=excluded.username,protected=excluded.protected,checked_at=excluded.checked_at`, p.ID, p.Username, p.Protected, now())
	return err
}
func (s *Store) SavePosts(target string, posts []Post) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	seen := map[string]bool{}
	var save func(Post, int) error
	save = func(p Post, depth int) error {
		if seen[p.ID] || depth > MaxQuoteDepth {
			return nil
		}
		seen[p.ID] = true
		if p.Quote != nil && p.Quote.ID != p.ID {
			p.QuoteID = p.Quote.ID
		}
		t := now()
		_, err = tx.Exec(`INSERT INTO posts VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET
 author_id=excluded.author_id,username=excluded.username,text=excluded.text,created_at=excluded.created_at,
 url=excluded.url,reply_to=excluded.reply_to,quote_id=excluded.quote_id,repost_id=excluded.repost_id,
 raw_json=excluded.raw_json,last_seen=excluded.last_seen`, p.ID, p.AuthorID, p.Username, p.Text, p.CreatedAt, p.URL, p.ReplyTo, p.QuoteID, p.RepostID, string(p.Raw), t, t)
		if err != nil {
			return err
		}
		for _, a := range p.Assets {
			_, err = tx.Exec(`INSERT INTO media(post_id,media_id,kind,source_url,updated_at) VALUES(?,?,?,?,?)
 ON CONFLICT(post_id,media_id) DO UPDATE SET source_url=excluded.source_url,kind=excluded.kind,updated_at=excluded.updated_at`, p.ID, a.ID, a.Kind, a.URL, t)
			if err != nil {
				return err
			}
		}
		if p.Quote != nil {
			return save(*p.Quote, depth+1)
		}
		return nil
	}
	for _, p := range posts {
		if err := save(p, 0); err != nil {
			return err
		}
		if target != "" {
			if _, err := tx.Exec("INSERT OR IGNORE INTO target_posts VALUES(?,?)", target, p.ID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// BackfillQuotes recovers embedded quotes from existing raw JSON without fetching
// extra pages. Batches are restart-safe and do not add quote authors as targets.
func (s *Store) BackfillQuotes() error {
	if _, err := s.DB.Exec("CREATE TABLE IF NOT EXISTS archive_migrations (name TEXT PRIMARY KEY)"); err != nil {
		return err
	}
	var done int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM archive_migrations WHERE name='quotes_v1'").Scan(&done); err != nil {
		return err
	}
	if done != 0 {
		return nil
	}
	after := ""
	for {
		rows, err := s.DB.Query("SELECT id,raw_json FROM posts WHERE id>? AND instr(raw_json,'quoted_status_result')>0 ORDER BY id LIMIT 100", after)
		if err != nil {
			return err
		}
		var batch []Post
		count := 0
		for rows.Next() {
			var raw string
			if err := rows.Scan(&after, &raw); err != nil {
				rows.Close()
				return err
			}
			count++
			var v object
			if json.Unmarshal([]byte(raw), &v) == nil {
				if p, err := parsePost(v); err == nil && p.Quote != nil {
					batch = append(batch, p)
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if err := s.SavePosts("", batch); err != nil {
			return err
		}
		if count < 100 {
			break
		}
	}
	_, err := s.DB.Exec("INSERT OR IGNORE INTO archive_migrations VALUES('quotes_v1')")
	return err
}

type MediaJob struct {
	PostID, ID, Kind, URL, Path, Status string
	StorageID, ObjectKey                string
	Bytes                               int64
	SHA256                              string
}

func (s *Store) MediaJobs(target string, includeDone bool) ([]MediaJob, error) {
	query := `SELECT post_id,media_id,kind,source_url,local_path,status,bytes,sha256,storage_id,object_key FROM media WHERE 1=1`
	var args []any
	if !includeDone {
		query += " AND status != 'done'"
	}
	if target != "" {
		query += ` AND post_id IN (WITH RECURSIVE wanted(id) AS (
 SELECT post_id FROM target_posts WHERE target=?
 UNION SELECT p.quote_id FROM posts p JOIN wanted w ON p.id=w.id WHERE p.quote_id<>''
 ) SELECT id FROM wanted)`
		args = append(args, target)
	}
	query += " ORDER BY post_id,media_id"
	r, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var jobs []MediaJob
	for r.Next() {
		var j MediaJob
		if err = r.Scan(&j.PostID, &j.ID, &j.Kind, &j.URL, &j.Path, &j.Status, &j.Bytes, &j.SHA256, &j.StorageID, &j.ObjectKey); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, r.Err()
}
func (s *Store) MediaResult(j MediaJob, path string, size int64, hash string, downloadErr error) error {
	if j.StorageID == "" {
		j.StorageID = "local"
	}
	status, message := "done", ""
	if downloadErr != nil {
		status = "failed"
		message = downloadErr.Error()
	}
	_, err := s.DB.Exec(`UPDATE media SET status=?,local_path=?,bytes=?,sha256=?,storage_id=?,object_key=?,attempts=attempts+1,last_error=?,updated_at=? WHERE post_id=? AND media_id=?`, status, path, size, hash, j.StorageID, j.ObjectKey, message, now(), j.PostID, j.ID)
	return err
}

func (s *Store) Status() (map[string]any, error) {
	result := map[string]any{}
	for _, table := range []string{"posts", "profiles", "media"} {
		var n int
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			return nil, err
		}
		result[table] = n
	}
	r, err := s.DB.Query("SELECT status,COUNT(*) FROM media GROUP BY status")
	if err != nil {
		return nil, err
	}
	m := map[string]int{}
	for r.Next() {
		var status string
		var n int
		if err = r.Scan(&status, &n); err != nil {
			r.Close()
			return nil, err
		}
		m[status] = n
	}
	err = r.Err()
	r.Close()
	if err != nil {
		return nil, err
	}
	result["media_status"] = m
	r, err = s.DB.Query("SELECT id,target,status,posts_seen,message FROM runs ORDER BY id DESC LIMIT 10")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var runs []map[string]any
	for r.Next() {
		var id, count int
		var target, status, message string
		if err = r.Scan(&id, &target, &status, &count, &message); err != nil {
			return nil, err
		}
		runs = append(runs, map[string]any{"id": id, "target": target, "status": status, "posts_seen": count, "message": message})
	}
	result["recent_runs"] = runs
	return result, r.Err()
}

func (s *Store) Export(target string, limit int) ([]map[string]any, error) {
	q := `SELECT id,username,text,created_at,url FROM posts`
	var args []any
	if target != "" {
		q += ` WHERE id IN (SELECT post_id FROM target_posts WHERE target=?)`
		args = append(args, target)
	}
	q += ` ORDER BY length(id) DESC,id DESC LIMIT ?`
	args = append(args, limit)
	r, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	posts := []map[string]any{}
	for r.Next() {
		var id, user, text, date, url string
		if err = r.Scan(&id, &user, &text, &date, &url); err != nil {
			return nil, err
		}
		posts = append(posts, map[string]any{"id": id, "username": user, "text": text, "created_at": date, "url": url})
	}
	return posts, r.Err()
}

func JSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("JSON 编码失败: %v", err)
	}
	return string(b)
}

// Existing archives retain local storage. Inspect before ALTER to make reopening idempotent.
func migrateStorage(db *sql.DB) error {
	rows, err := db.Query("PRAGMA table_info(media)")
	if err != nil {
		return err
	}
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def any
		if err = rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		cols[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, name := range []string{"storage_id", "object_key"} {
		if !cols[name] {
			def := "''"
			if name == "storage_id" {
				def = "'local'"
			}
			if _, err = db.Exec("ALTER TABLE media ADD COLUMN " + name + " TEXT NOT NULL DEFAULT " + def); err != nil {
				return err
			}
		}
	}
	return nil
}
