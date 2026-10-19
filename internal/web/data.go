package web

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"xm/internal/archive"
	"xm/internal/database"
)

type Monitor struct {
	UserID           string          `json:"user_id"`
	CurrentUsername  string          `json:"current_username"`
	Username         string          `json:"username"`
	Interval         int             `json:"interval_seconds"`
	Replies          bool            `json:"replies"`
	Enabled          bool            `json:"enabled"`
	Requested        bool            `json:"requested"`
	Running          bool            `json:"running"`
	NextRun          int64           `json:"next_run"`
	LastStarted      int64           `json:"last_started"`
	LastFinished     int64           `json:"last_finished"`
	LastSuccess      int64           `json:"last_success"`
	LastError        string          `json:"last_error"`
	Failures         int             `json:"failures"`
	Posts            int             `json:"posts"`
	LastPosts        int             `json:"last_posts"`
	NewPosts         int             `json:"new_posts"`
	LastStatus       string          `json:"last_status"`
	Catchup          bool            `json:"catchup"`
	CatchupPost      string          `json:"-"`
	CatchupReply     string          `json:"-"`
	CatchupScrolls   int             `json:"-"`
	BaselineStatus   string          `json:"baseline_status"`
	PostsBaseline    string          `json:"posts_baseline_status"`
	RepliesBaseline  string          `json:"replies_baseline_status"`
	BaselineStarted  int64           `json:"baseline_started"`
	BaselineFinished int64           `json:"baseline_finished"`
	BaselineStop     string          `json:"baseline_stop_reason,omitempty"`
	Checkpoint       *CheckpointView `json:"checkpoint,omitempty"`
}

type CheckpointView struct {
	Phase     string `json:"phase"`
	ReachedAt string `json:"reached_at,omitempty"`
	SavedAt   int64  `json:"saved_at,omitempty"`
}

const incompleteCollectionMessage = "本轮已到达安全边界，系统将在后续批次继续追赶旧归档位置"

func displayCollectionMessage(message string) string {
	// Preserve the historical run, but explain this one known pre-upgrade
	// internal-parameter failure without exposing a meaningless implementation name.
	if message == "limit 必须在 1～10000 之间" {
		return "旧版本内部采集上限配置冲突，任务未启动；升级后已修复"
	}
	return message
}

func migrate(s *archive.Store) error {
	if s.DB.Dialect == "mysql" {
		return MigrateStore(s.DB)
	}
	_, err := s.DB.Exec(`CREATE TABLE IF NOT EXISTS monitors (
 username TEXT PRIMARY KEY, replies INTEGER NOT NULL DEFAULT 0, enabled INTEGER NOT NULL DEFAULT 1, requested INTEGER NOT NULL DEFAULT 0,
 running INTEGER NOT NULL DEFAULT 0, next_run INTEGER NOT NULL, last_started INTEGER NOT NULL DEFAULT 0,
 last_finished INTEGER NOT NULL DEFAULT 0, last_success INTEGER NOT NULL DEFAULT 0,
 last_error TEXT NOT NULL DEFAULT '', failures INTEGER NOT NULL DEFAULT 0,
 last_posts INTEGER NOT NULL DEFAULT 0, new_posts INTEGER NOT NULL DEFAULT 0,
 catchup INTEGER NOT NULL DEFAULT 0, catchup_post_anchor TEXT NOT NULL DEFAULT '',
 catchup_reply_anchor TEXT NOT NULL DEFAULT '', catchup_scroll_budget INTEGER NOT NULL DEFAULT 0,
 baseline_status TEXT NOT NULL DEFAULT 'not_started', posts_baseline_status TEXT NOT NULL DEFAULT 'not_started',
 replies_baseline_status TEXT NOT NULL DEFAULT 'not_enabled', baseline_started INTEGER NOT NULL DEFAULT 0,
 baseline_finished INTEGER NOT NULL DEFAULT 0, baseline_stop_reason TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS web_state (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS collection_jobs (
 id INTEGER PRIMARY KEY, target TEXT NOT NULL, source TEXT NOT NULL, status TEXT NOT NULL,
 queued_at INTEGER NOT NULL, queue_order INTEGER NOT NULL, started_at INTEGER NOT NULL DEFAULT 0, finished_at INTEGER NOT NULL DEFAULT 0,
 run_id INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0, retry_at INTEGER NOT NULL DEFAULT 0,
 retry_count INTEGER NOT NULL DEFAULT 0, message TEXT NOT NULL DEFAULT '',
 mode TEXT NOT NULL DEFAULT 'incremental'
);
CREATE INDEX IF NOT EXISTS collection_jobs_queue ON collection_jobs(finished_at,status,queue_order,id);
CREATE INDEX IF NOT EXISTS collection_jobs_target ON collection_jobs(target,finished_at);
CREATE INDEX IF NOT EXISTS monitors_due ON monitors(next_run);
CREATE INDEX IF NOT EXISTS target_posts_post ON target_posts(post_id);`)
	if err != nil {
		return err
	}
	if err = ensureRunJobColumn(s.DB); err != nil {
		return err
	}
	if err = ensureCollectionModeSchema(s.DB); err != nil {
		return err
	}
	return migrateNotifications(s.DB)
}

const monitorColumns = `COALESCE((SELECT user_id FROM target_identities i WHERE i.target=m.username),''),COALESCE((SELECT current_username FROM target_identities i WHERE i.target=m.username),m.username),m.username,COALESCE((SELECT default_interval_seconds FROM system_settings WHERE id=1),0),m.replies,m.enabled,m.requested,m.running,m.next_run,m.last_started,m.last_finished,m.last_success,m.last_error,m.failures,m.last_posts,m.new_posts,(SELECT COUNT(*) FROM target_posts t WHERE t.target=m.username),COALESCE((SELECT status FROM runs r WHERE r.target=m.username ORDER BY r.id DESC LIMIT 1),''),m.catchup,m.catchup_post_anchor,m.catchup_reply_anchor,m.catchup_scroll_budget,m.baseline_status,m.posts_baseline_status,m.replies_baseline_status,m.baseline_started,m.baseline_finished,m.baseline_stop_reason,COALESCE((SELECT state FROM collection_progress c WHERE c.target=m.username),'')`

type scanner interface{ Scan(...any) error }

func scanMonitor(row scanner) (Monitor, error) {
	var m Monitor
	var checkpoint string
	err := row.Scan(&m.UserID, &m.CurrentUsername, &m.Username, &m.Interval, &m.Replies, &m.Enabled, &m.Requested, &m.Running, &m.NextRun, &m.LastStarted, &m.LastFinished, &m.LastSuccess, &m.LastError, &m.Failures, &m.LastPosts, &m.NewPosts, &m.Posts, &m.LastStatus, &m.Catchup, &m.CatchupPost, &m.CatchupReply, &m.CatchupScrolls, &m.BaselineStatus, &m.PostsBaseline, &m.RepliesBaseline, &m.BaselineStarted, &m.BaselineFinished, &m.BaselineStop, &checkpoint)
	if err == nil {
		m.LastError = displayCollectionMessage(m.LastError)
	}
	if err == nil && checkpoint != "" {
		var progress archive.CollectionProgress
		if json.Unmarshal([]byte(checkpoint), &progress) == nil {
			phase := "posts"
			reachedAt := progress.Posts.ReachedAt
			if m.Replies && !progress.Replies.Done {
				phase = "replies"
				reachedAt = progress.Replies.ReachedAt
			}
			m.Checkpoint = &CheckpointView{Phase: phase, ReachedAt: reachedAt, SavedAt: progress.SavedAt}
		}
	}
	return m, err
}
func monitors(db *database.DB) ([]Monitor, error) {
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
	return nil
}
func state(db *database.DB, key string) string {
	var v string
	_ = db.QueryRow("SELECT value FROM web_state WHERE `key`=?", key).Scan(&v)
	return v
}
func setState(db *database.DB, key, value string) error {
	_, err := db.Exec(database.Upsert(db.Dialect, "INSERT INTO web_state VALUES(?,?)", []string{"key"}, []string{"value"}), key, value)
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
	ID               string      `json:"id"`
	Username         string      `json:"username"`
	Name             string      `json:"name"`
	Text             string      `json:"text"`
	CreatedAt        string      `json:"created_at"`
	SavedAt          string      `json:"saved_at"`
	Protected        bool        `json:"protected"`
	ReplyTo          string      `json:"reply_to"`
	RepostID         string      `json:"repost_id"`
	QuoteID          string      `json:"quote_id"`
	Quote            *PostView   `json:"quote,omitempty"`
	QuoteUnavailable bool        `json:"quote_unavailable"`
	Media            []MediaView `json:"media"`
}
type PostList struct {
	Items  []PostView `json:"items"`
	Total  int        `json:"total"`
	Offset int        `json:"offset"`
	Limit  int        `json:"limit"`
}

// A quote remains attached to its parent; it is not an independent timeline row.
// UNION (rather than UNION ALL) also terminates cycles in old or malformed data.
const relatedPosts = `WITH RECURSIVE related(root,id) AS (
 SELECT id,id FROM posts
 UNION SELECT r.root,p.quote_id FROM related r JOIN posts p ON p.id=r.id WHERE p.quote_id<>''
) `
const postColumns = `p.id,p.username,p.text,p.created_at,p.first_seen,p.reply_to,p.repost_id,p.quote_id,COALESCE((SELECT protected FROM profiles u WHERE u.id=p.author_id),0),p.raw_json`

func scanPost(row scanner) (PostView, error) {
	var p PostView
	var raw string
	err := row.Scan(&p.ID, &p.Username, &p.Text, &p.CreatedAt, &p.SavedAt, &p.ReplyTo, &p.RepostID, &p.QuoteID, &p.Protected, &raw)
	p.Name, _ = archive.AuthorDetails([]byte(raw))
	if p.Name == "" {
		p.Name = p.Username
	}
	p.Media = []MediaView{}
	return p, err
}
func attachPost(db *database.DB, p *PostView, depth int, seen map[string]bool) error {
	seen[p.ID] = true
	defer delete(seen, p.ID)
	rows, err := db.Query("SELECT media_id,kind,status,last_error,bytes FROM media WHERE post_id=? ORDER BY media_id", p.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var m MediaView
		if err = rows.Scan(&m.ID, &m.Kind, &m.Status, &m.Error, &m.Bytes); err != nil {
			rows.Close()
			return err
		}
		p.Media = append(p.Media, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if p.QuoteID != "" {
		p.QuoteUnavailable = true
		if depth >= archive.MaxQuoteDepth || seen[p.QuoteID] {
			return nil
		}
		q, err := scanPost(db.QueryRow("SELECT "+postColumns+" FROM posts p WHERE p.id=?", p.QuoteID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = attachPost(db, &q, depth+1, seen); err != nil {
			return err
		}
		p.Quote = &q
		p.QuoteUnavailable = false
	}
	return nil
}
func listPosts(db *database.DB, target, query, kind, section string, offset, limit int) (PostList, error) {
	result := PostList{Items: []PostView{}, Offset: offset, Limit: limit}
	where := " WHERE EXISTS (SELECT 1 FROM target_posts t WHERE t.post_id=p.id)"
	args := []any{}
	if target != "" {
		where += " AND EXISTS (SELECT 1 FROM target_posts t WHERE t.post_id=p.id AND t.target=?)"
		args = append(args, target)
	}
	if query != "" {
		where += ` AND EXISTS (SELECT 1 FROM related r JOIN posts q ON q.id=r.id WHERE r.root=p.id AND (instr(lower(q.text),lower(?))>0 OR instr(lower(q.username),lower(?))>0))`
		args = append(args, query, query)
	}
	media := `EXISTS (SELECT 1 FROM related r JOIN media a ON a.post_id=r.id WHERE r.root=p.id`
	switch section {
	case "posts":
		where += " AND p.reply_to=''"
	case "replies":
		where += " AND p.reply_to<>''"
	case "media":
		where += " AND " + media + ")"
	}
	switch kind {
	case "photo":
		where += " AND " + media + " AND a.kind='photo')"
	case "video":
		where += " AND " + media + " AND a.kind IN ('video','animated_gif'))"
	case "media":
		where += " AND " + media + ")"
	case "text":
		where += " AND NOT " + media + ")"
	}
	if err := db.QueryRow(relatedPosts+"SELECT COUNT(*) FROM posts p"+where, args...).Scan(&result.Total); err != nil {
		return result, err
	}
	args = append(args, limit, offset)
	rows, err := db.Query(relatedPosts+"SELECT "+postColumns+" FROM posts p"+where+" ORDER BY length(p.id) DESC,p.id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		p, e := scanPost(rows)
		if e != nil {
			rows.Close()
			return result, e
		}
		result.Items = append(result.Items, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for i := range result.Items {
		if err = attachPost(db, &result.Items[i], 0, map[string]bool{}); err != nil {
			return result, err
		}
	}
	return result, nil
}

type UserView struct {
	UserID          string `json:"user_id"`
	AvatarURL       string `json:"avatar_url"`
	CurrentUsername string `json:"current_username"`
	Username        string `json:"username"`
	Name            string `json:"name"`
	Bio             string `json:"bio"`
	Protected       bool   `json:"protected"`
	Posts           int    `json:"posts"`
	Media           int    `json:"media"`
	LastSaved       string `json:"last_saved"`
	LastRun         int64  `json:"last_run"`
	Monitored       bool   `json:"monitored"`
	Enabled         bool   `json:"enabled"`
	Interval        int    `json:"interval_seconds"`
}

func archiveUsers(db *database.DB) ([]UserView, error) {
	rows, err := db.Query(`WITH users(username) AS (SELECT target FROM target_posts UNION SELECT username FROM monitors)
 SELECT u.username,COUNT(t.post_id),COALESCE(MAX(p.first_seen),''),m.username IS NOT NULL,COALESCE(m.enabled,0),COALESCE((SELECT default_interval_seconds FROM system_settings WHERE id=1),0),COALESCE(m.last_finished,0)
 FROM users u LEFT JOIN target_posts t ON t.target=u.username LEFT JOIN posts p ON p.id=t.post_id LEFT JOIN monitors m ON m.username=u.username GROUP BY u.username,m.username,m.enabled,m.last_finished ORDER BY u.username`)
	if err != nil {
		return nil, err
	}
	users := []UserView{}
	for rows.Next() {
		var u UserView
		if err = rows.Scan(&u.Username, &u.Posts, &u.LastSaved, &u.Monitored, &u.Enabled, &u.Interval, &u.LastRun); err != nil {
			rows.Close()
			return nil, err
		}
		users = append(users, u)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range users {
		u := &users[i]
		u.CurrentUsername = u.Username
		eIdentity := db.QueryRow("SELECT user_id,current_username FROM target_identities WHERE target=?", u.Username).Scan(&u.UserID, &u.CurrentUsername)
		if eIdentity != nil && !errors.Is(eIdentity, sql.ErrNoRows) {
			return nil, eIdentity
		}
		u.Name = u.CurrentUsername
		var raw string
		e := db.QueryRow(`SELECT raw_json,COALESCE((SELECT protected FROM profiles WHERE id=p.author_id),0) FROM posts p JOIN target_posts t ON t.post_id=p.id WHERE t.target=? ORDER BY length(p.id) DESC,p.id DESC LIMIT 1`, u.Username).Scan(&raw, &u.Protected)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		name, bio := archive.AuthorDetails([]byte(raw))
		if name != "" {
			u.Name = name
		}
		u.Bio = bio
		if err = db.QueryRow(relatedPosts+`SELECT COUNT(*) FROM media a WHERE status='done' AND EXISTS (SELECT 1 FROM related r JOIN target_posts t ON t.post_id=r.root WHERE t.target=? AND r.id=a.post_id)`, u.Username).Scan(&u.Media); err != nil {
			return nil, err
		}
	}
	return users, nil
}

func recentRuns(db *database.DB) ([]map[string]any, error) {
	r, err := db.Query("SELECT r.id,r.target,r.started_at,COALESCE(r.finished_at,''),r.status,COALESCE(m.posts_read,r.posts_seen),r.message,m.new_posts,r.job_id,COALESCE(j.source,'legacy'),COALESCE(j.queued_at,0),COALESCE(j.mode,'incremental') FROM runs r LEFT JOIN run_metrics m ON m.run_id=r.id LEFT JOIN collection_jobs j ON j.id=r.job_id ORDER BY r.id DESC LIMIT 20")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	result := []map[string]any{}
	for r.Next() {
		var id, posts int
		var jobID, queuedAt int64
		var added sql.NullInt64
		var target, start, end, status, message, source, mode string
		if err = r.Scan(&id, &target, &start, &end, &status, &posts, &message, &added, &jobID, &source, &queuedAt, &mode); err != nil {
			return nil, err
		}
		message = displayCollectionMessage(message)
		result = append(result, map[string]any{"id": id, "target": target, "started_at": start, "finished_at": end, "status": status, "posts": posts, "message": message, "new_posts": nullableCount(added), "job_id": jobID, "source": source, "queued_at": queuedAt, "mode": mode})
	}
	return result, r.Err()
}

func counts(db *database.DB) (map[string]int64, error) {
	result := map[string]int64{}
	queries := map[string]string{"posts": "SELECT COUNT(*) FROM posts p WHERE EXISTS (SELECT 1 FROM target_posts t WHERE t.post_id=p.id)", "media": "SELECT COUNT(*) FROM media WHERE status='done'", "media_failed": "SELECT COUNT(*) FROM media WHERE status IN ('failed','permanent')", "media_bytes": "SELECT COALESCE(SUM(bytes),0) FROM media WHERE status='done'", "monitors": "SELECT COUNT(*) FROM monitors", "active": "SELECT COUNT(*) FROM monitors WHERE enabled=1", "today": "SELECT COUNT(*) FROM posts p WHERE first_seen>=? AND EXISTS (SELECT 1 FROM target_posts t WHERE t.post_id=p.id)"}
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
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		return "数据库操作失败，请检查 MySQL 连接、权限与容量"
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

func (a *App) missingMonitor(result sql.Result, name string) error {
	if err := missing(result); err == nil {
		return nil
	}
	var count int
	if err := a.store.DB.QueryRow("SELECT COUNT(*) FROM monitors WHERE username=?", name).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("监控账号不存在")
	}
	return nil
}

func nullableCount(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}
