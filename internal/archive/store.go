package archive

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"xm/internal/database"

	_ "modernc.org/sqlite"
)

type Store struct{ DB *database.DB }

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
CREATE TABLE IF NOT EXISTS x_cookies (id INTEGER PRIMARY KEY CHECK(id=1),auth_token TEXT NOT NULL,ct0 TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS posts (
 id TEXT PRIMARY KEY, author_id TEXT NOT NULL, username TEXT NOT NULL, text TEXT NOT NULL,
 created_at TEXT NOT NULL, url TEXT NOT NULL, reply_to TEXT NOT NULL, quote_id TEXT NOT NULL,
 repost_id TEXT NOT NULL, raw_json TEXT NOT NULL, first_seen TEXT NOT NULL, last_seen TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS target_posts (
 target TEXT NOT NULL, post_id TEXT NOT NULL REFERENCES posts(id), PRIMARY KEY(target,post_id)
);
CREATE TABLE IF NOT EXISTS target_identities (target TEXT PRIMARY KEY,user_id TEXT NOT NULL,current_username TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS profiles (
 id TEXT PRIMARY KEY, username TEXT NOT NULL, protected INTEGER NOT NULL, checked_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS media (
 post_id TEXT NOT NULL REFERENCES posts(id), media_id TEXT NOT NULL, kind TEXT NOT NULL,
 source_url TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', local_path TEXT NOT NULL DEFAULT '',
 bytes INTEGER NOT NULL DEFAULT 0, sha256 TEXT NOT NULL DEFAULT '', attempts INTEGER NOT NULL DEFAULT 0,
 next_attempt INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL,
 storage_id TEXT NOT NULL DEFAULT 'local', object_key TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(post_id,media_id)
);
CREATE INDEX IF NOT EXISTS media_status ON media(status,next_attempt);
CREATE TABLE IF NOT EXISTS runs (
 id INTEGER PRIMARY KEY, target TEXT NOT NULL, started_at TEXT NOT NULL, finished_at TEXT,
 status TEXT NOT NULL, posts_seen INTEGER NOT NULL DEFAULT 0, message TEXT NOT NULL DEFAULT ''
);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	d := &database.DB{DB: db, Dialect: "sqlite"}
	if err = migrateSettings(d); err != nil {
		db.Close()
		return nil, err
	}
	if err = ensureArchiveIndexes(d); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: d}, nil
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
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = EndRunTx(tx, id, status, count, message); err != nil {
		return err
	}
	return tx.Commit()
}

// EndRunTx allows the scheduler to commit the run result and checkpoint together.
func EndRunTx(tx *database.Tx, id int64, status string, count int, message string) error {
	if _, err := tx.Exec("UPDATE runs SET finished_at=?,status=?,posts_seen=?,message=? WHERE id=?", now(), status, count, message, id); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM runs WHERE status<>'running' AND id < (SELECT COALESCE(MIN(id),0) FROM (SELECT id FROM runs ORDER BY id DESC LIMIT 1000) recent)")
	return err
}
func (s *Store) SaveProfile(p Profile) error {
	_, err := s.DB.Exec(database.Upsert(s.DB.Dialect, `INSERT INTO profiles VALUES(?,?,?,?)`, []string{"id"}, []string{"username", "protected", "checked_at"}), p.ID, p.Username, p.Protected, now())
	return err
}
func (s *Store) SavePosts(target string, posts []Post) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	type storedPost struct{ authorID, username, text, createdAt, postURL, replyTo, quoteID, repostID, raw string }
	existing := map[string]storedPost{}
	ids := []string{}
	indexed := map[string]bool{}
	var index func(Post, int)
	index = func(p Post, depth int) {
		if p.ID == "" || indexed[p.ID] || depth > MaxQuoteDepth {
			return
		}
		indexed[p.ID] = true
		ids = append(ids, p.ID)
		if p.Quote != nil {
			index(*p.Quote, depth+1)
		}
	}
	for _, p := range posts {
		index(p, 0)
	}
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		args := make([]any, end-start)
		for i, id := range ids[start:end] {
			args[i] = id
		}
		rows, queryErr := tx.Query(`SELECT id,author_id,username,text,created_at,url,reply_to,quote_id,repost_id,raw_json FROM posts WHERE id IN (`+strings.TrimRight(strings.Repeat("?,", len(args)), ",")+`)`, args...)
		if queryErr != nil {
			return queryErr
		}
		for rows.Next() {
			var id string
			var old storedPost
			if queryErr = rows.Scan(&id, &old.authorID, &old.username, &old.text, &old.createdAt, &old.postURL, &old.replyTo, &old.quoteID, &old.repostID, &old.raw); queryErr != nil {
				break
			}
			existing[id] = old
		}
		if queryErr == nil {
			queryErr = rows.Err()
		}
		rows.Close()
		if queryErr != nil {
			return queryErr
		}
	}
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
		raw := string(p.Raw)
		old, exists := existing[p.ID]
		unchanged := exists && old.authorID == p.AuthorID && old.username == p.Username && old.text == p.Text && old.createdAt == p.CreatedAt && old.postURL == p.URL && old.replyTo == p.ReplyTo && old.quoteID == p.QuoteID && old.repostID == p.RepostID && old.raw == raw
		if unchanged {
			// Preserve last_seen semantics without rewriting the large raw payload or
			// touching already indexed media rows.
			_, err = tx.Exec("UPDATE posts SET last_seen=? WHERE id=?", t, p.ID)
		} else {
			_, err = tx.Exec(database.Upsert(s.DB.Dialect, `INSERT INTO posts VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, []string{"id"}, []string{"author_id", "username", "text", "created_at", "url", "reply_to", "quote_id", "repost_id", "raw_json", "last_seen"}), p.ID, p.AuthorID, p.Username, p.Text, p.CreatedAt, p.URL, p.ReplyTo, p.QuoteID, p.RepostID, raw, t, t)
		}
		if err != nil {
			return err
		}
		if !unchanged {
			for _, a := range p.Assets {
				result, updateErr := tx.Exec(`UPDATE media SET kind=?,status=CASE WHEN source_url<>? THEN 'pending' ELSE status END,attempts=CASE WHEN source_url<>? THEN 0 ELSE attempts END,next_attempt=CASE WHEN source_url<>? THEN 0 ELSE next_attempt END,last_error=CASE WHEN source_url<>? THEN '' ELSE last_error END,source_url=?,updated_at=? WHERE post_id=? AND media_id=?`, a.Kind, a.URL, a.URL, a.URL, a.URL, a.URL, t, p.ID, a.ID)
				if updateErr != nil {
					return updateErr
				}
				changed, updateErr := result.RowsAffected()
				if updateErr != nil {
					return updateErr
				}
				if changed == 0 {
					_, err = tx.InsertIgnore("INSERT INTO media(post_id,media_id,kind,source_url,updated_at) VALUES(?,?,?,?,?)", "post_id", p.ID, a.ID, a.Kind, a.URL, t)
				}
				if err != nil {
					return err
				}
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
			if _, err := tx.InsertIgnore("INSERT INTO target_posts VALUES(?,?)", "target", target, p.ID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

type MediaJob struct {
	AuthorID, Username, Directory       string
	PostID, ID, Kind, URL, Path, Status string
	StorageID, ObjectKey                string
	Bytes                               int64
	SHA256                              string
	Attempts                            int
	NextAttempt                         int64
}

func (s *Store) MediaJobs(target string, includeDone bool) ([]MediaJob, error) {
	query := `SELECT post_id,media_id,kind,source_url,local_path,status,bytes,sha256,storage_id,object_key,posts.author_id,posts.username,attempts,next_attempt FROM media JOIN posts ON posts.id=media.post_id WHERE 1=1`
	var args []any
	if !includeDone {
		query += " AND (status='pending' OR (status='failed' AND next_attempt<=?))"
		args = append(args, time.Now().Unix())
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
		if err = r.Scan(&j.PostID, &j.ID, &j.Kind, &j.URL, &j.Path, &j.Status, &j.Bytes, &j.SHA256, &j.StorageID, &j.ObjectKey, &j.AuthorID, &j.Username, &j.Attempts, &j.NextAttempt); err != nil {
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
	nextAttempt := int64(0)
	if downloadErr != nil {
		status = "failed"
		message = downloadErr.Error()
		if IsPermanentMediaError(downloadErr) {
			status = "permanent"
		} else {
			delay := 5 * time.Minute * time.Duration(1<<min(j.Attempts, 8))
			nextAttempt = time.Now().Add(min(delay, 24*time.Hour)).Unix()
		}
	}
	_, err := s.DB.Exec(`UPDATE media SET status=?,local_path=?,bytes=?,sha256=?,storage_id=?,object_key=?,attempts=attempts+1,next_attempt=?,last_error=?,updated_at=? WHERE post_id=? AND media_id=?`, status, path, size, hash, j.StorageID, j.ObjectKey, nextAttempt, message, now(), j.PostID, j.ID)
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
