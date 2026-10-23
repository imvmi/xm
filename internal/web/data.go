package web

import (
	"context"
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
	ShowInTimeline   bool            `json:"show_in_timeline"`
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
	QueuedJob        *CollectionJob  `json:"queued_job,omitempty"`
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

const monitorColumns = `COALESCE((SELECT user_id FROM target_identities i WHERE i.target=m.username),''),COALESCE((SELECT current_username FROM target_identities i WHERE i.target=m.username),m.username),m.username,COALESCE((SELECT default_interval_seconds FROM system_settings WHERE id=1),0),m.replies,m.enabled,m.requested,m.running,m.next_run,m.last_started,m.last_finished,m.last_success,m.last_error,m.failures,m.last_posts,m.new_posts,(SELECT COUNT(*) FROM target_posts t WHERE t.target=m.username),COALESCE((SELECT status FROM runs r WHERE r.target=m.username ORDER BY r.id DESC LIMIT 1),''),m.catchup,m.catchup_post_anchor,m.catchup_reply_anchor,m.catchup_scroll_budget,m.baseline_status,m.posts_baseline_status,m.replies_baseline_status,m.baseline_started,m.baseline_finished,m.baseline_stop_reason,COALESCE((SELECT state FROM collection_progress c WHERE c.target=m.username),''),m.show_in_timeline`

type scanner interface{ Scan(...any) error }

func scanMonitor(row scanner) (Monitor, error) {
	var m Monitor
	var checkpoint string
	err := row.Scan(&m.UserID, &m.CurrentUsername, &m.Username, &m.Interval, &m.Replies, &m.Enabled, &m.Requested, &m.Running, &m.NextRun, &m.LastStarted, &m.LastFinished, &m.LastSuccess, &m.LastError, &m.Failures, &m.LastPosts, &m.NewPosts, &m.Posts, &m.LastStatus, &m.Catchup, &m.CatchupPost, &m.CatchupReply, &m.CatchupScrolls, &m.BaselineStatus, &m.PostsBaseline, &m.RepliesBaseline, &m.BaselineStarted, &m.BaselineFinished, &m.BaselineStop, &checkpoint, &m.ShowInTimeline)
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
	return monitorList(db, true)
}

func monitorList(db *database.DB, stats bool) ([]Monitor, error) {
	columns := monitorColumns
	if !stats {
		// Runtime polling must not count an account's full archive every six seconds.
		columns = strings.Replace(columns, "(SELECT COUNT(*) FROM target_posts t WHERE t.target=m.username)", "-1", 1)
	}
	r, err := db.Query("SELECT " + columns + " FROM monitors m ORDER BY m.username")
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
	// Best-effort display only. Control flow must use readState/loadSchedulerState.
	v, _ := readState(db, key)
	return v
}
func readState(db *database.DB, key string) (string, error) {
	var v string
	err := db.QueryRow("SELECT value FROM web_state WHERE `key`=?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}
func setState(db *database.DB, key, value string) error {
	_, err := db.Exec(database.Upsert(db.Dialect, "INSERT INTO web_state VALUES(?,?)", []string{"key"}, []string{"value"}), key, value)
	return err
}

type MediaView struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Error   string `json:"error"`
	Bytes   int64  `json:"bytes"`
	Version string `json:"version,omitempty"`
}
type PostView struct {
	AuthorID         string        `json:"-"`
	ArchiveTarget    string        `json:"archive_target,omitempty"`
	AvatarURL        string        `json:"avatar_url,omitempty"`
	ID               string        `json:"id"`
	Username         string        `json:"username"`
	Name             string        `json:"name"`
	Text             string        `json:"text"`
	CreatedAt        string        `json:"created_at"`
	SavedAt          string        `json:"saved_at"`
	Bookmarked       bool          `json:"bookmarked"`
	BookmarkedAt     int64         `json:"bookmarked_at"`
	Protected        bool          `json:"protected"`
	SelfReply        bool          `json:"self_reply"`
	ReplyToAuthorID  string        `json:"reply_to_author_id,omitempty"`
	HasReplies       bool          `json:"has_replies,omitempty"`
	ReplyPreview     *ReplySummary `json:"reply_preview,omitempty"`
	ReplyTo          string        `json:"reply_to"`
	RepostID         string        `json:"repost_id"`
	QuoteID          string        `json:"quote_id"`
	Quote            *PostView     `json:"quote,omitempty"`
	QuoteUnavailable bool          `json:"quote_unavailable"`
	Media            []MediaView   `json:"media"`
}
type PostList struct {
	Items      []PostView `json:"items"`
	Total      int        `json:"total"`
	Offset     int        `json:"offset"`
	Limit      int        `json:"limit"`
	NextCursor string     `json:"next_cursor"`
	HasMore    bool       `json:"has_more"`
}

const replyAuthorSQL = "COALESCE(NULLIF(p.reply_to_author_id,''),(SELECT parent.author_id FROM posts parent WHERE parent.id=p.reply_to),'')"

// Express the root predicate without making SQLite prefer posts_reply over the
// timeline ordering index; this keeps the first page free of a full sort.
const rootPostSQL = "length(p.reply_to)=0"

const postColumns = `p.id,p.username,p.text,p.created_at,p.first_seen,p.reply_to,p.repost_id,p.quote_id,COALESCE((SELECT protected FROM profiles u WHERE u.id=p.author_id),0),p.raw_json,p.author_id,COALESCE((SELECT MIN(target) FROM target_posts WHERE post_id=p.id),''),COALESCE((SELECT created_at FROM post_bookmarks WHERE post_id=p.id),0)` + `,` + replyAuthorSQL + `,EXISTS(SELECT 1 FROM posts child WHERE child.reply_to=p.id AND child.id<>p.id)`

func scanPost(row scanner) (PostView, error) {
	var p PostView
	var raw string
	err := row.Scan(&p.ID, &p.Username, &p.Text, &p.CreatedAt, &p.SavedAt, &p.ReplyTo, &p.RepostID, &p.QuoteID, &p.Protected, &raw, &p.AuthorID, &p.ArchiveTarget, &p.BookmarkedAt, &p.ReplyToAuthorID, &p.HasReplies)
	p.Bookmarked = p.BookmarkedAt > 0
	p.SelfReply = p.ReplyTo != "" && p.AuthorID != "" && p.AuthorID == p.ReplyToAuthorID
	p.Name, _ = archive.AuthorDetails([]byte(raw))
	if p.Name == "" {
		p.Name = p.Username
	}
	p.Media = []MediaView{}
	return p, err
}
func listPosts(ctx context.Context, db *database.DB, target, query, kind, section string, offset, limit int) (PostList, error) {
	return listPostsPage(ctx, db, target, query, kind, section, offset, limit, "", true)
}

func listPostsPage(ctx context.Context, db *database.DB, target, query, kind, section string, offset, limit int, before string, count bool) (PostList, error) {
	if section == "bookmarks" {
		return listBookmarkedPosts(ctx, db, target, query, kind, offset, limit, before, count)
	}
	result := PostList{Items: []PostView{}, Offset: offset, Limit: limit}
	from := " FROM posts p"
	orderID := "p.id"
	// Visibility applies to archive roots on the home feed, not to an account's
	// own archive or embedded quotes. A root shared by visible/hidden archives
	// remains visible once. Archives without a monitor remain visible by default.
	where := " WHERE EXISTS (SELECT 1 FROM target_posts t LEFT JOIN monitors m ON m.username=t.target LEFT JOIN account_preferences ap ON ap.target=t.target WHERE t.post_id=p.id AND COALESCE(m.show_in_timeline,ap.show_in_timeline,1)=1)"
	args := []any{}
	if target != "" {
		from = " FROM target_posts t JOIN posts p ON p.id=t.post_id"
		orderID = "t.post_id"
		where = " WHERE t.target=?"
		args = append(args, target)
	} else {
		// Replies belong to their parent thread and never become independent
		// entries on the home timeline.
		where += " AND " + rootPostSQL
	}
	switch section {
	case "posts":
		where += " AND " + rootPostSQL
	case "replies":
		where += " AND p.reply_to<>''"
	}
	filter, filterArgs := postFilter(query, kind, section == "media")
	where += filter
	args = append(args, filterArgs...)
	result.Total = -1
	if count {
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*)"+from+where, args...).Scan(&result.Total); err != nil {
			return result, err
		}
	}
	// Seek both parts of the compound index. An OR over different ID lengths
	// can scan every same-length ID before the cursor on SQLite/MySQL.
	ranges := timelineRanges(orderID, before)
	if before != "" {
		offset = 0
	}
	for _, span := range ranges {
		pageArgs := append(append([]any{}, args...), span.args...)
		pageArgs = append(pageArgs, limit+1-len(result.Items), offset)
		rows, err := db.QueryContext(ctx, "SELECT "+postColumns+from+where+span.clause+" ORDER BY "+span.order+" LIMIT ? OFFSET ?", pageArgs...)
		if err != nil {
			return result, err
		}
		for rows.Next() {
			p, err := scanPost(rows)
			if err != nil {
				rows.Close()
				return result, err
			}
			result.Items = append(result.Items, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return result, err
		}
		if len(result.Items) > limit {
			break
		}
	}
	result.HasMore = len(result.Items) > limit
	if result.HasMore {
		result.Items = result.Items[:limit]
	}
	if len(result.Items) > 0 {
		result.NextCursor = result.Items[len(result.Items)-1].ID
	}
	if err := attachPosts(ctx, db, result.Items); err != nil {
		return result, err
	}
	return result, nil
}

type UserView struct {
	ShowInTimeline  bool   `json:"show_in_timeline"`
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
	StatsPending    bool   `json:"stats_pending,omitempty"`
}

func archiveUsers(db *database.DB) ([]UserView, error) {
	return archiveUsersFor(db, "")
}
func archiveUsersFor(db *database.DB, target string) ([]UserView, error) {
	users, err := archiveUserDirectory(db, target)
	if err != nil {
		return nil, err
	}
	stats, err := archiveUserStats(db, target)
	if err != nil {
		return nil, err
	}
	byUser := make(map[string]ArchiveUserStats, len(stats))
	for _, s := range stats {
		byUser[s.Username] = s
	}
	for i := range users {
		u := &users[i]
		s := byUser[u.Username]
		u.Posts, u.Media, u.LastSaved, u.StatsPending = s.Posts, s.Media, s.LastSaved, false
	}
	return users, nil
}

func recentRuns(db *database.DB) ([]map[string]any, error) {
	r, err := db.Query("SELECT r.id,r.target,r.started_at,COALESCE(r.finished_at,''),r.status,COALESCE(m.posts_read,r.posts_seen),r.message,m.new_posts,r.job_id,COALESCE(j.source,'legacy'),COALESCE(j.queued_at,0),COALESCE(j.mode,'incremental') FROM runs r LEFT JOIN run_metrics m ON m.run_id=r.id LEFT JOIN collection_jobs j ON j.id=r.job_id WHERE r.status<>'running' ORDER BY r.id DESC LIMIT 20")
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
