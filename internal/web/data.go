package web

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"xm/internal/archive"
)

type Monitor struct {
	Username     string `json:"username"`
	Interval     int    `json:"interval_seconds"`
	Limit        int    `json:"limit"`
	Replies      bool   `json:"replies"`
	Enabled      bool   `json:"enabled"`
	Requested    bool   `json:"requested"`
	Running      bool   `json:"running"`
	NextRun      int64  `json:"next_run"`
	LastStarted  int64  `json:"last_started"`
	LastFinished int64  `json:"last_finished"`
	LastSuccess  int64  `json:"last_success"`
	LastError    string `json:"last_error"`
	Failures     int    `json:"failures"`
	Posts        int    `json:"posts"`
	LastPosts    int    `json:"last_posts"`
	NewPosts     int    `json:"new_posts"`
}

func migrate(s *archive.Store) error {
	_, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS monitors (
 username TEXT PRIMARY KEY, interval_seconds INTEGER NOT NULL, post_limit INTEGER NOT NULL,
 replies INTEGER NOT NULL DEFAULT 0, enabled INTEGER NOT NULL DEFAULT 1, requested INTEGER NOT NULL DEFAULT 0,
 running INTEGER NOT NULL DEFAULT 0, next_run INTEGER NOT NULL, last_started INTEGER NOT NULL DEFAULT 0,
 last_finished INTEGER NOT NULL DEFAULT 0, last_success INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '', failures INTEGER NOT NULL DEFAULT 0,
 last_posts INTEGER NOT NULL DEFAULT 0, new_posts INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS web_state (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS monitors_due ON monitors(next_run);
CREATE INDEX IF NOT EXISTS target_posts_post ON target_posts(post_id);`)
	return err
}

const monitorColumns = `m.username,m.interval_seconds,m.post_limit,m.replies,m.enabled,m.requested,m.running,m.next_run,m.last_started,m.last_finished,m.last_success,m.last_error,m.failures,m.last_posts,m.new_posts,(SELECT COUNT(*) FROM target_posts t WHERE t.target=m.username)`

type scanner interface{ Scan(...any) error }

func scanMonitor(row scanner) (Monitor, error) {
	var m Monitor
	err := row.Scan(&m.Username, &m.Interval, &m.Limit, &m.Replies, &m.Enabled, &m.Requested, &m.Running, &m.NextRun, &m.LastStarted, &m.LastFinished, &m.LastSuccess, &m.LastError, &m.Failures, &m.LastPosts, &m.NewPosts, &m.Posts)
	return m, err
}
func monitors(db *sql.DB) ([]Monitor, error) {
	r, err := db.Query("SELECT " + monitorColumns + " FROM monitors m ORDER BY m.username")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	list := []Monitor{}
	for r.Next() {
		m, e := scanMonitor(r)
		if e != nil {
			return nil, e
		}
		list = append(list, m)
	}
	return list, r.Err()
}
func validateMonitor(m *Monitor) error {
	u, err := archive.Username(m.Username)
	if err != nil {
		return err
	}
	m.Username = u
	if m.Interval < 60 || m.Interval > 604800 {
		return errors.New("轮询周期必须为 1 分钟到 7 天")
	}
	if m.Limit < 1 || m.Limit > 1000 {
		return errors.New("每轮采集上限必须为 1～1000 条")
	}
	return nil
}
func state(db *sql.DB, key string) string {
	var v string
	_ = db.QueryRow("SELECT value FROM web_state WHERE key=?", key).Scan(&v)
	return v
}
func setState(db *sql.DB, key, value string) error {
	_, err := db.Exec("INSERT INTO web_state VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}

type MediaView struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Error  string `json:"error"`
	Bytes  int64  `json:"bytes"`
}
type PostView struct {
	ID        string      `json:"id"`
	Username  string      `json:"username"`
	Text      string      `json:"text"`
	CreatedAt string      `json:"created_at"`
	SavedAt   string      `json:"saved_at"`
	Protected bool        `json:"protected"`
	ReplyTo   string      `json:"reply_to"`
	RepostID  string      `json:"repost_id"`
	Media     []MediaView `json:"media"`
}
type PostList struct {
	Items  []PostView `json:"items"`
	Total  int        `json:"total"`
	Offset int        `json:"offset"`
	Limit  int        `json:"limit"`
}

func listPosts(db *sql.DB, target, query, kind string, offset, limit int) (PostList, error) {
	result := PostList{Items: []PostView{}, Offset: offset, Limit: limit}
	where := " WHERE 1=1"
	args := []any{}
	if target != "" {
		where += " AND EXISTS (SELECT 1 FROM target_posts t WHERE t.post_id=p.id AND t.target=?)"
		args = append(args, target)
	}
	if query != "" {
		where += " AND (instr(lower(p.text),lower(?))>0 OR instr(lower(p.username),lower(?))>0)"
		args = append(args, query, query)
	}
	switch kind {
	case "photo":
		where += " AND EXISTS (SELECT 1 FROM media a WHERE a.post_id=p.id AND a.kind='photo')"
	case "video":
		where += " AND EXISTS (SELECT 1 FROM media a WHERE a.post_id=p.id AND a.kind IN ('video','animated_gif'))"
	case "text":
		where += " AND NOT EXISTS (SELECT 1 FROM media a WHERE a.post_id=p.id)"
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM posts p"+where, args...).Scan(&result.Total); err != nil {
		return result, err
	}
	args = append(args, limit, offset)
	r, err := db.Query(`SELECT p.id,p.username,p.text,p.created_at,p.first_seen,p.reply_to,p.repost_id,COALESCE((SELECT protected FROM profiles u WHERE u.id=p.author_id),0) FROM posts p`+where+" ORDER BY length(p.id) DESC,p.id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return result, err
	}
	for r.Next() {
		var p PostView
		if err = r.Scan(&p.ID, &p.Username, &p.Text, &p.CreatedAt, &p.SavedAt, &p.ReplyTo, &p.RepostID, &p.Protected); err != nil {
			r.Close()
			return result, err
		}
		p.Media = []MediaView{}
		result.Items = append(result.Items, p)
	}
	err = r.Err()
	r.Close()
	if err != nil {
		return result, err
	}
	// Close the post cursor before media queries: SQLite uses one DB connection.
	for i, p := range result.Items {
		r, err := db.Query("SELECT media_id,kind,status,last_error,bytes FROM media WHERE post_id=? ORDER BY media_id", p.ID)
		if err != nil {
			return result, err
		}
		for r.Next() {
			var m MediaView
			if err = r.Scan(&m.ID, &m.Kind, &m.Status, &m.Error, &m.Bytes); err != nil {
				r.Close()
				return result, err
			}
			result.Items[i].Media = append(result.Items[i].Media, m)
		}
		err = r.Err()
		r.Close()
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func recentRuns(db *sql.DB) ([]map[string]any, error) {
	r, err := db.Query("SELECT id,target,started_at,COALESCE(finished_at,''),status,posts_seen,message FROM runs ORDER BY id DESC LIMIT 20")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	result := []map[string]any{}
	for r.Next() {
		var id, posts int
		var target, start, end, status, message string
		if err = r.Scan(&id, &target, &start, &end, &status, &posts, &message); err != nil {
			return nil, err
		}
		result = append(result, map[string]any{"id": id, "target": target, "started_at": start, "finished_at": end, "status": status, "posts": posts, "message": message})
	}
	return result, r.Err()
}

func counts(db *sql.DB) (map[string]int64, error) {
	result := map[string]int64{}
	queries := map[string]string{"posts": "SELECT COUNT(*) FROM posts", "media": "SELECT COUNT(*) FROM media WHERE status='done'", "media_failed": "SELECT COUNT(*) FROM media WHERE status='failed'", "media_bytes": "SELECT COALESCE(SUM(bytes),0) FROM media WHERE status='done'", "monitors": "SELECT COUNT(*) FROM monitors", "active": "SELECT COUNT(*) FROM monitors WHERE enabled=1", "today": "SELECT COUNT(*) FROM posts WHERE first_seen>=?"}
	for key, q := range queries {
		var n int64
		var args []any
		if key == "today" {
			args = append(args, time.Now().UTC().Format("2006-01-02"))
		}
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			return nil, err
		}
		result[key] = n
	}
	return result, nil
}

func safeMessage(err error) string {
	// Collector messages are intentionally sanitized at their source. Database
	// failures are not suitable for public diagnostics and never include payloads.
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 500 {
		s = s[:500]
	}
	if strings.Contains(s, "SQL") || strings.Contains(s, "constraint") {
		return "数据库操作失败，请检查磁盘空间与权限"
	}
	return s
}
func missing(rows sql.Result) error {
	n, err := rows.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("监控账号不存在")
	}
	return nil
}
