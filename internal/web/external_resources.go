package web

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"xm/internal/archive"
)

func (a *App) externalCanonical(name string) (string, error) {
	name, err := archive.Username(name)
	if err != nil {
		return "", err
	}
	var target string
	err = a.store.DB.QueryRow("SELECT target FROM target_identities WHERE target=? OR current_username=? ORDER BY CASE WHEN target=? THEN 0 ELSE 1 END,target LIMIT 1", name, name, name).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return name, nil
	}
	return target, err
}
func apiPage(r *http.Request) (int, string, error) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 100 {
			return 0, "", errors.New("limit 必须在 1～100 之间")
		}
		limit = n
	}
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > 128 {
		return 0, "", errors.New("无效游标")
	}
	return limit, cursor, nil
}
func apiAccountFilter(c apiCredential, column string) (string, []any) {
	if c.AllAccounts {
		return "1=1", nil
	}
	if len(c.Accounts) == 0 {
		return "1=0", nil
	}
	args := make([]any, len(c.Accounts))
	for i, v := range c.Accounts {
		args[i] = v
	}
	return column + " IN (" + strings.TrimRight(strings.Repeat("?,", len(args)), ",") + ")", args
}

type externalAccountView struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	UserID      string `json:"user_id"`
	Monitored   bool   `json:"monitored"`
	Enabled     bool   `json:"enabled"`
	Replies     bool   `json:"include_replies"`
	Baseline    string `json:"baseline_status"`
	Running     bool   `json:"running"`
	Queued      bool   `json:"queued"`
	NextRun     int64  `json:"next_run"`
	LastSuccess int64  `json:"last_success"`
}

func (a *App) externalAccountRows(c apiCredential, target, cursor string, limit int) ([]externalAccountView, error) {
	filter, args := apiAccountFilter(c, "u.username")
	query := `WITH users(username) AS (SELECT target FROM target_posts UNION SELECT username FROM monitors)
 SELECT u.username,COALESCE(i.current_username,u.username),COALESCE(i.user_id,''),m.username IS NOT NULL,COALESCE(m.enabled,0),COALESCE(m.replies,0),COALESCE(m.baseline_status,'not_started'),COALESCE(m.running,0),EXISTS(SELECT 1 FROM collection_jobs j WHERE j.target=u.username AND j.finished_at=0 AND j.status<>'running'),COALESCE(m.next_run,0),COALESCE(m.last_success,0)
 FROM users u LEFT JOIN target_identities i ON i.target=u.username LEFT JOIN monitors m ON m.username=u.username WHERE ` + filter
	if target != "" {
		query += " AND u.username=?"
		args = append(args, target)
	}
	if cursor != "" {
		query += " AND u.username>?"
		args = append(args, cursor)
	}
	query += " ORDER BY u.username LIMIT ?"
	args = append(args, limit)
	rows, err := a.store.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []externalAccountView{}
	for rows.Next() {
		var v externalAccountView
		if err = rows.Scan(&v.ID, &v.Username, &v.UserID, &v.Monitored, &v.Enabled, &v.Replies, &v.Baseline, &v.Running, &v.Queued, &v.NextRun, &v.LastSuccess); err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}
func (a *App) externalAccounts(w http.ResponseWriter, r *http.Request, c apiCredential) {
	limit, cursor, err := apiPage(r)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	items, err := a.externalAccountRows(c, "", cursor, limit+1)
	if err != nil {
		problem(w, 500, "读取账号失败")
		return
	}
	more := len(items) > limit
	next := ""
	if more {
		items = items[:limit]
		next = items[len(items)-1].ID
	}
	respond(w, 200, map[string]any{"items": items, "has_more": more, "next_cursor": next})
}
func (a *App) externalAccount(w http.ResponseWriter, r *http.Request, c apiCredential) {
	items, err := a.externalAccountRows(c, r.PathValue("account"), "", 1)
	if err != nil {
		problem(w, 500, "读取账号失败")
		return
	}
	if len(items) == 0 {
		problem(w, 404, "账号不存在")
		return
	}
	respond(w, 200, items[0])
}
func apiJSONRequest(r *http.Request, value any) *http.Request {
	raw, _ := json.Marshal(value)
	clone := r.Clone(r.Context())
	clone.Body = io.NopCloser(bytes.NewReader(raw))
	clone.ContentLength = int64(len(raw))
	clone.Header.Set("Content-Type", "application/json")
	return clone
}
func copyAPIResult(w http.ResponseWriter, b *apiBuffer) {
	for k, v := range b.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}
func (a *App) externalCreateAccount(w http.ResponseWriter, r *http.Request, c apiCredential) {
	var in struct {
		Username string `json:"username"`
		Enabled  bool   `json:"enabled"`
		Replies  bool   `json:"include_replies"`
	}
	if !decode(w, r, &in) {
		return
	}
	target, err := a.externalCanonical(in.Username)
	if err != nil {
		problem(w, 400, "无效账号名")
		return
	}
	if !c.account(target) {
		problem(w, 404, "账号未授权，请管理员先加入凭据范围")
		return
	}
	b := newAPIBuffer()
	a.addMonitor(b, apiJSONRequest(r, monitorInput{Username: target, Enabled: in.Enabled, Replies: in.Replies}))
	if b.status >= 400 {
		copyAPIResult(w, b)
		return
	}
	respond(w, 201, map[string]any{"id": target, "username": in.Username, "enabled": in.Enabled, "include_replies": in.Replies})
}
func (a *App) externalEditAccount(w http.ResponseWriter, r *http.Request, c apiCredential) {
	var in struct {
		Enabled *bool `json:"enabled"`
		Replies *bool `json:"include_replies"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Enabled == nil && in.Replies == nil {
		problem(w, 400, "请提供 enabled 或 include_replies")
		return
	}
	target := r.PathValue("account")
	var enabled, replies bool
	if err := a.store.DB.QueryRow("SELECT enabled,replies FROM monitors WHERE username=?", target).Scan(&enabled, &replies); err != nil {
		problem(w, 404, "监控账号不存在")
		return
	}
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if in.Replies != nil {
		replies = *in.Replies
	}
	request := apiJSONRequest(r, monitorInput{Username: target, Enabled: enabled, Replies: replies})
	request.SetPathValue("username", target)
	a.editMonitor(w, request)
}
func (a *App) externalDeleteAccount(w http.ResponseWriter, r *http.Request, c apiCredential) {
	r.SetPathValue("username", r.PathValue("account"))
	a.deleteMonitor(w, r)
}

type externalJobView struct {
	ID            string `json:"id"`
	AccountID     string `json:"account_id"`
	RequestedMode string `json:"requested_mode"`
	Mode          string `json:"mode"`
	Status        string `json:"status"`
	QueuedAt      int64  `json:"queued_at"`
	StartedAt     int64  `json:"started_at"`
	FinishedAt    int64  `json:"finished_at"`
	RetryAt       int64  `json:"retry_at"`
	Attempts      int    `json:"attempts"`
	Message       string `json:"message"`
	Phase         string `json:"phase,omitempty"`
	PostsRead     int64  `json:"posts_read"`
	NewPosts      int64  `json:"new_posts"`
	internalID    int64
	runID         int64
}

const externalJobColumns = "public_id,target,requested_mode,mode,status,queued_at,started_at,finished_at,retry_at,attempts,message,id,run_id"

func scanExternalJob(row scanner) (externalJobView, error) {
	var j externalJobView
	err := row.Scan(&j.ID, &j.AccountID, &j.RequestedMode, &j.Mode, &j.Status, &j.QueuedAt, &j.StartedAt, &j.FinishedAt, &j.RetryAt, &j.Attempts, &j.Message, &j.internalID, &j.runID)
	j.Message = displayCollectionMessage(j.Message)
	return j, err
}
func (a *App) externalJob(c apiCredential, id string) (externalJobView, error) {
	j, err := scanExternalJob(a.store.DB.QueryRow("SELECT "+externalJobColumns+" FROM collection_jobs WHERE public_id=? AND api_credential_id=?", id, c.ID))
	if err == nil && !c.account(j.AccountID) {
		err = sql.ErrNoRows
	}
	return j, err
}
func (a *App) externalTask(w http.ResponseWriter, r *http.Request, c apiCredential) {
	j, err := a.externalJob(c, r.PathValue("task"))
	if err != nil {
		problem(w, 404, "任务不存在、已清理或未授权")
		return
	}
	_ = a.store.DB.QueryRow("SELECT posts_read,new_posts FROM run_metrics WHERE run_id=?", j.runID).Scan(&j.PostsRead, &j.NewPosts)
	if j.Status == "running" {
		progress := CollectionJob{ID: j.internalID}
		a.scheduler.describeProgress(&progress)
		j.Phase = progress.Phase
	}
	respond(w, 200, j)
}
func (a *App) externalTasks(w http.ResponseWriter, r *http.Request, c apiCredential) {
	limit, cursor, err := apiPage(r)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	var before int64
	if cursor != "" {
		j, e := a.externalJob(c, cursor)
		if e != nil {
			problem(w, 400, "游标无效或对应记录已清理")
			return
		}
		before = j.internalID
	}
	filter, args := apiAccountFilter(c, "target")
	query := "SELECT " + externalJobColumns + " FROM collection_jobs WHERE " + filter + " AND api_credential_id=?"
	args = append(args, c.ID)
	if before > 0 {
		query += " AND id<?"
		args = append(args, before)
	}
	if account := r.URL.Query().Get("account_id"); account != "" {
		if !c.account(account) {
			problem(w, 404, "账号未授权")
			return
		}
		query += " AND target=?"
		args = append(args, account)
	}
	if status := r.URL.Query().Get("status"); status != "" {
		if len(status) > 32 {
			problem(w, 400, "无效状态")
			return
		}
		query += " AND status=?"
		args = append(args, status)
	}
	for _, bound := range []struct{ name, op string }{{"created_after", ">="}, {"created_before", "<="}} {
		if raw := r.URL.Query().Get(bound.name); raw != "" {
			value, e := strconv.ParseInt(raw, 10, 64)
			if e != nil || value < 0 {
				problem(w, 400, "时间筛选必须为非负 Unix 秒数")
				return
			}
			query += " AND queued_at" + bound.op + "?"
			args = append(args, value)
		}
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)
	rows, err := a.store.DB.Query(query, args...)
	if err != nil {
		problem(w, 500, "读取任务失败")
		return
	}
	defer rows.Close()
	items := []externalJobView{}
	for rows.Next() {
		j, e := scanExternalJob(rows)
		if e != nil {
			problem(w, 500, "读取任务失败")
			return
		}
		items = append(items, j)
	}
	if rows.Err() != nil {
		problem(w, 500, "读取任务失败")
		return
	}
	more := len(items) > limit
	next := ""
	if more {
		items = items[:limit]
		next = items[len(items)-1].ID
	}
	respond(w, 200, map[string]any{"items": items, "has_more": more, "next_cursor": next})
}
func (a *App) submitExternalTask(w http.ResponseWriter, r *http.Request, c apiCredential, target, mode string) {
	mode, err := normalizeCollectionMode(mode)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	if mode == collectionModeRescan && !c.allows("tasks:rescan") {
		problem(w, 403, "重新扫描需要 tasks:rescan 权限")
		return
	}
	var active int
	if err = a.store.DB.QueryRow("SELECT COUNT(*) FROM collection_jobs WHERE api_credential_id=? AND finished_at=0", c.ID).Scan(&active); err != nil {
		problem(w, 500, "读取任务限额失败")
		return
	}
	if active >= c.QueueLimit {
		problem(w, 429, "未结束任务数超过凭据限额")
		return
	}
	id, err := a.scheduler.queueAPI(target, mode, c.ID)
	if err != nil {
		problem(w, 409, safeMessage(err))
		return
	}
	j, err := scanExternalJob(a.store.DB.QueryRow("SELECT "+externalJobColumns+" FROM collection_jobs WHERE id=?", id))
	if err != nil {
		problem(w, 500, "任务已提交，读取结果失败")
		return
	}
	respond(w, 202, j)
}
func (a *App) externalCreateTask(w http.ResponseWriter, r *http.Request, c apiCredential) {
	var in struct {
		Mode string `json:"mode"`
	}
	if !decode(w, r, &in) {
		return
	}
	a.submitExternalTask(w, r, c, r.PathValue("account"), in.Mode)
}
func (a *App) externalRetryTask(w http.ResponseWriter, r *http.Request, c apiCredential) {
	j, err := a.externalJob(c, r.PathValue("task"))
	if err != nil {
		problem(w, 404, "任务不存在")
		return
	}
	if j.FinishedAt == 0 || (j.Status != "failed" && j.Status != "incomplete" && j.Status != "cancelled") {
		problem(w, 409, "只能重试已结束的失败、未完成或取消任务")
		return
	}
	mode := j.Mode
	if mode == collectionModeRescan {
		mode = collectionModeBackfill
	} // Continue the checkpoint instead of resetting it again.
	a.submitExternalTask(w, r, c, j.AccountID, mode)
}
func (a *App) externalCancelTask(w http.ResponseWriter, r *http.Request, c apiCredential) {
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	j, err := a.externalJob(c, r.PathValue("task"))
	if err != nil {
		problem(w, 404, "任务不存在")
		return
	}
	if j.FinishedAt != 0 {
		problem(w, 409, "任务已结束")
		return
	}
	if j.Status == "running" {
		current, _ := a.scheduler.Current()
		if current != j.AccountID {
			problem(w, 409, "任务状态正在变化，请重新查询")
			return
		}
		// Leave enabled unchanged. The scheduler finishes and persists the cancellation.
		if _, err = a.store.DB.Exec("UPDATE monitors SET requested=0,next_run=? WHERE username=?", time.Now().Unix()+int64(a.Config().DefaultInterval), j.AccountID); err != nil {
			problem(w, 500, "停止任务失败")
			return
		}
		a.scheduler.Cancel(j.AccountID)
		respond(w, 202, map[string]any{"id": j.ID, "cancel_requested": true})
		return
	}
	if _, err = cancelWaitingCollectionJob(a.store.DB, j.internalID); err != nil {
		problem(w, 409, "任务已开始或状态发生变化，请重新查询")
		return
	}
	a.scheduler.Wake()
	respond(w, 200, map[string]any{"id": j.ID, "status": "cancelled"})
}

func sanitizeExternalPost(p *PostView, account string) {
	// Reply ancestors are not part of the credential's quote authorization tree.
	p.ReplyPreview = nil
	p.HasReplies = false
	p.ArchiveTarget = account
	p.Bookmarked = false
	p.BookmarkedAt = 0
	p.AvatarURL = ""
	if p.Quote != nil {
		sanitizeExternalPost(p.Quote, account)
	}
}
func (a *App) externalTimeline(w http.ResponseWriter, r *http.Request, c apiCredential) {
	limit, cursor, err := apiPage(r)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	if cursor != "" && !validPostID(cursor) {
		problem(w, 400, "无效时间线游标")
		return
	}
	q := r.URL.Query()
	section := q.Get("section")
	switch section {
	case "", "posts", "replies", "media":
	default:
		problem(w, 400, "section 必须为 posts、replies 或 media")
		return
	}
	if len(q.Get("q")) > 500 {
		problem(w, 400, "搜索内容过长")
		return
	}
	switch q.Get("kind") {
	case "", "photo", "video", "animated_gif":
	default:
		problem(w, 400, "无效媒体类型")
		return
	}
	target := r.PathValue("account")
	items, err := a.externalAccountRows(c, target, "", 1)
	if err != nil {
		problem(w, 500, "读取账号失败")
		return
	}
	if len(items) == 0 {
		problem(w, 404, "账号不存在")
		return
	}
	page, err := listPostsPage(a.store.DB, target, q.Get("q"), q.Get("kind"), section, 0, limit, cursor, q.Get("count") == "1")
	if err != nil {
		problem(w, 500, "读取时间线失败")
		return
	}
	for i := range page.Items {
		sanitizeExternalPost(&page.Items[i], target)
	}
	respond(w, 200, page)
}
func (a *App) externalPostAllowed(c apiCredential, id string) (bool, error) {
	if !validPostID(id) {
		return false, nil
	}
	filter, accountArgs := apiAccountFilter(c, "t.target")
	// Walk indexed reverse quote links from this post instead of scanning every
	// post in the caller's archive on each media request. UNION terminates cycles.
	query := `WITH RECURSIVE ancestors(id) AS (SELECT id FROM posts WHERE id=? UNION SELECT p.id FROM ancestors a JOIN posts p ON p.quote_id=a.id)
 SELECT COUNT(*) FROM ancestors a JOIN target_posts t ON t.post_id=a.id WHERE ` + filter
	args := append([]any{id}, accountArgs...)
	var n int
	err := a.store.DB.QueryRow(query, args...).Scan(&n)
	return n > 0, err
}
func (a *App) externalPost(w http.ResponseWriter, r *http.Request, c apiCredential) {
	id := r.PathValue("post")
	allowed, err := a.externalPostAllowed(c, id)
	if err != nil {
		problem(w, 500, "读取权限失败")
		return
	}
	if !allowed {
		problem(w, 404, "推文不存在或未授权")
		return
	}
	p, err := scanPost(a.store.DB.QueryRow("SELECT "+postColumns+" FROM posts p WHERE p.id=?", id))
	if err != nil {
		problem(w, 404, "推文不存在")
		return
	}
	items := []PostView{p}
	if err = attachPosts(a.store.DB, items); err != nil {
		problem(w, 500, "读取推文失败")
		return
	}
	sanitizeExternalPost(&items[0], "")
	respond(w, 200, items[0])
}
func (a *App) externalMedia(w http.ResponseWriter, r *http.Request, c apiCredential) {
	allowed, err := a.externalPostAllowed(c, r.PathValue("post"))
	if err != nil {
		apiError(w, 500, "internal_error", "读取权限失败")
		return
	}
	if !allowed {
		apiError(w, 404, "not_found", "媒体不存在或未授权")
		return
	}
	a.media(w, r)
}
func (a *App) externalAvatar(w http.ResponseWriter, r *http.Request, c apiCredential) {
	items, err := a.externalAccountRows(c, r.PathValue("account"), "", 1)
	if err != nil || len(items) == 0 || items[0].UserID == "" {
		apiError(w, 404, "not_found", "头像不存在")
		return
	}
	r.SetPathValue("user", items[0].UserID)
	a.avatar(w, r)
}
