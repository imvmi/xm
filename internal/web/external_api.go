package web

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type apiRoute struct {
	Pattern, Scope, Summary string
	Idempotent              bool
	Handler                 func(*App, http.ResponseWriter, *http.Request, apiCredential)
}

var externalRoutes = []apiRoute{
	{"GET /api/v1/me", "", "查看当前凭据权限与限额", false, (*App).externalMe},
	{"GET /api/v1/accounts", "accounts:read", "列出授权账号", false, (*App).externalAccounts},
	{"POST /api/v1/accounts", "accounts:create", "添加监控账号", true, (*App).externalCreateAccount},
	{"GET /api/v1/accounts/{account}", "accounts:read", "查看账号详情", false, (*App).externalAccount},
	{"PATCH /api/v1/accounts/{account}", "accounts:update", "修改账号监控配置", false, (*App).externalEditAccount},
	{"DELETE /api/v1/accounts/{account}/monitor", "accounts:delete", "取消监控并保留归档", false, (*App).externalDeleteAccount},
	{"POST /api/v1/accounts/{account}/tasks", "", "提交增量、补采或重新扫描任务", true, (*App).externalCreateTask},
	{"GET /api/v1/tasks", "tasks:read", "列出本凭据提交的任务", false, (*App).externalTasks},
	{"GET /api/v1/tasks/{task}", "tasks:read", "查询任务进度与结果", false, (*App).externalTask},
	{"POST /api/v1/tasks/{task}/cancel", "tasks:cancel", "取消本次任务，保留自动监控设置", false, (*App).externalCancelTask},
	{"POST /api/v1/tasks/{task}/retry", "tasks:retry", "重试失败、未完成或取消的任务", true, (*App).externalRetryTask},
	{"GET /api/v1/accounts/{account}/timeline", "timeline:list", "读取已归档时间线及引用", false, (*App).externalTimeline},
	{"GET /api/v1/posts/{post}", "posts:read", "读取授权推文及引用", false, (*App).externalPost},
	{"GET /api/v1/posts/{post}/media/{asset}", "media:download", "读取授权媒体，支持 Range", false, (*App).externalMedia},
	{"GET /api/v1/accounts/{account}/avatar", "avatars:read", "读取授权账号头像", false, (*App).externalAvatar},
}

func (route apiRoute) allows(c apiCredential) bool {
	if route.Pattern == "POST /api/v1/accounts/{account}/tasks" {
		return c.allows(apiTaskScope(collectionModeIncremental)) || c.allows(apiTaskScope(collectionModeBackfill)) || c.allows(apiTaskScope(collectionModeRescan))
	}
	return route.Scope == "" || c.allows(route.Scope)
}

func (route apiRoute) binary() bool {
	return strings.HasSuffix(route.Pattern, "/avatar") || strings.HasSuffix(route.Pattern, "/media/{asset}")
}

// A small response buffer keeps legacy handlers reusable while normalizing JSON
// errors and durably caching idempotent replies. Media streams bypass this buffer.
type apiBuffer struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newAPIBuffer() *apiBuffer           { return &apiBuffer{header: make(http.Header)} }
func (b *apiBuffer) Header() http.Header { return b.header }
func (b *apiBuffer) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}
func (b *apiBuffer) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = 200
	}
	return b.body.Write(p)
}
func apiError(w http.ResponseWriter, status int, code, message string) {
	respond(w, status, map[string]any{"error": map[string]string{"code": code, "message": message, "request_id": w.Header().Get("X-Request-ID")}})
}
func externalErrorCode(status int) string {
	switch status {
	case 400, 415:
		return "invalid_request"
	case 401:
		return "invalid_credential"
	case 403:
		return "insufficient_scope"
	case 404:
		return "not_found"
	case 409:
		return "conflict"
	case 429:
		return "rate_limited"
	case 504:
		return "query_timeout"
	case 503:
		return "unavailable"
	default:
		return "internal_error"
	}
}
func (a *App) externalHandler() http.Handler {
	mux := http.NewServeMux()
	for _, route := range externalRoutes {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { a.serveExternalRoute(w, r, route) })
		if route.Scope == "timeline:list" || route.Scope == "posts:read" {
			handler = readTimeout(postReadTimeout, handler)
		}
		mux.HandleFunc(route.Pattern, handler)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", rand.Text())
		w.Header().Set("Cache-Control", "no-store")
		// Do not expose the session-based UI endpoints through Bearer authentication.
		_, pattern := mux.Handler(r)
		if pattern == "" {
			apiError(w, 404, "not_found", "接口不存在或请求方法不支持")
			return
		}
		// ServeMux.ServeHTTP populates path parameters; Handler alone does not.
		mux.ServeHTTP(w, r)
	})
}
func (a *App) serveExternalRoute(w http.ResponseWriter, r *http.Request, route apiRoute) {
	c, err := a.authenticateAPI(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		apiError(w, 401, "invalid_credential", "凭据无效、已过期、已停用或已撤销")
		return
	}
	if !route.allows(c) {
		required := route.Scope
		if required == "" {
			required = "tasks:submit:incremental、tasks:submit:backfill 或 tasks:submit:rescan"
		}
		apiError(w, 403, "insufficient_scope", "凭据没有所需权限："+required)
		return
	}
	taskRequest := route.Idempotent && strings.Contains(route.Pattern, "/tasks")
	if !a.allowAPIRequest(c, taskRequest) {
		w.Header().Set("Retry-After", strconv.FormatInt(60-time.Now().Unix()%60, 10))
		apiError(w, 429, "rate_limited", "调用频率超过凭据限额")
		return
	}
	mutation := r.Method != "GET" && r.Method != "HEAD"
	if mutation {
		if !a.writeGate.TryRLock() {
			apiError(w, 503, "maintenance", "数据迁移维护中")
			return
		}
		defer a.writeGate.RUnlock()
		if a.maintenance.Load() {
			apiError(w, 503, "maintenance", "数据迁移维护中")
			return
		}
		a.api.writeMu.Lock()
		defer a.api.writeMu.Unlock()
		// A request may have waited behind a credential revocation or scope edit.
		c, err = a.authenticateAPI(r)
		if err != nil {
			apiError(w, 401, "invalid_credential", "凭据已失效")
			return
		}
		if !route.allows(c) {
			apiError(w, 403, "insufficient_scope", "凭据权限已变化")
			return
		}
	}
	if target := r.PathValue("account"); target != "" && !c.account(target) {
		apiError(w, 404, "not_found", "账号不存在或未授权")
		return
	}
	if task := r.PathValue("task"); task != "" {
		_, err = a.externalJob(c, task)
		if err != nil {
			apiError(w, 404, "not_found", "任务不存在、已清理或未授权")
			return
		}
	}
	now := time.Now().Unix()
	if c.LastUsedAt < now-60 {
		if mutation {
			_, _ = a.store.DB.ExecContext(r.Context(), "UPDATE api_credentials SET last_used_at=? WHERE id=? AND last_used_at<?", now, c.ID, now-60)
		} else if a.writeGate.TryRLock() {
			if !a.maintenance.Load() {
				_, _ = a.store.DB.ExecContext(r.Context(), "UPDATE api_credentials SET last_used_at=? WHERE id=? AND last_used_at<?", now, c.ID, now-60)
			}
			a.writeGate.RUnlock()
		}
	}
	// Binary endpoints keep streaming and byte-range behavior, but never public cacheability.
	if route.binary() {
		route.Handler(a, &externalMediaWriter{ResponseWriter: w}, r, c)
		return
	}
	b := newAPIBuffer()
	b.Header().Set("X-Request-ID", w.Header().Get("X-Request-ID"))
	idemID := ""
	if route.Idempotent {
		key := r.Header.Get("Idempotency-Key")
		if len(key) < 1 || len(key) > 128 {
			apiError(w, 400, "idempotency_key_required", "请提供 1～128 字节的 Idempotency-Key")
			return
		}
		raw, e := io.ReadAll(io.LimitReader(r.Body, 65537))
		if e != nil || len(raw) > 65536 {
			apiError(w, 400, "invalid_request", "请求内容过大或不可读")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if route.Pattern == "POST /api/v1/accounts" {
			var in struct {
				Username string `json:"username"`
			}
			if json.Unmarshal(raw, &in) == nil {
				target, e := a.externalCanonical(in.Username)
				if e == nil && !c.account(target) {
					apiError(w, 404, "not_found", "账号未授权；请管理员先将账号加入凭据范围")
					return
				}
			}
		}
		// Check mode grants before replaying a cached result, including after a
		// credential has been narrowed since the original request.
		if taskRequest {
			mode := ""
			if r.PathValue("task") != "" {
				job, e := a.externalJob(c, r.PathValue("task"))
				if e != nil {
					apiError(w, 404, "not_found", "任务不存在、已清理或未授权")
					return
				}
				mode = retryExternalTaskMode(job)
			} else {
				var in struct {
					Mode string `json:"mode"`
				}
				if json.Unmarshal(raw, &in) != nil {
					apiError(w, 400, "invalid_request", "无效任务请求")
					return
				}
				mode = in.Mode
			}
			mode, e = normalizeCollectionMode(mode)
			if e != nil {
				apiError(w, 400, "invalid_request", e.Error())
				return
			}
			if !c.allows(apiTaskScope(mode)) {
				apiError(w, 403, "insufficient_scope", "凭据没有所需权限："+apiTaskScope(mode))
				return
			}
		}
		idemID = apiDigest(c.ID + "\x00" + key)
		hash := apiDigest(r.Method + "\x00" + r.URL.Path + "\x00" + string(raw))
		if _, err = a.store.DB.Exec("DELETE FROM api_idempotency WHERE expires_at<=?", now); err != nil {
			apiError(w, 503, "unavailable", "幂等记录暂不可用")
			return
		}
		var storedHash, response string
		var status int
		err = a.store.DB.QueryRow("SELECT request_hash,status_code,response FROM api_idempotency WHERE id=?", idemID).Scan(&storedHash, &status, &response)
		if err == nil {
			if hash != storedHash {
				apiError(w, 409, "idempotency_conflict", "同一幂等键不能用于不同请求")
				return
			}
			if status == 0 {
				apiError(w, 409, "idempotency_pending", "此请求正在处理或结果待确认，请查询账号或任务状态，不要换键重复提交")
				return
			}
			w.Header().Set("Idempotency-Replayed", "true")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, response)
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			apiError(w, 503, "unavailable", "读取幂等记录失败")
			return
		}
		var count int
		if err = a.store.DB.QueryRow("SELECT COUNT(*) FROM api_idempotency WHERE credential_id=?", c.ID).Scan(&count); err != nil {
			apiError(w, 503, "unavailable", "读取幂等记录失败")
			return
		}
		if count >= 1000 {
			apiError(w, 429, "idempotency_limit", "每个凭据最多保留 1000 个有效幂等键，请等待 24 小时保留期结束")
			return
		}
		if _, err = a.store.DB.Exec("INSERT INTO api_idempotency(id,credential_id,request_hash,status_code,response,expires_at) VALUES(?,?,?,0,'',?)", idemID, c.ID, hash, now+86400); err != nil {
			apiError(w, 503, "unavailable", "保存幂等记录失败")
			return
		}
	}
	route.Handler(a, b, r, c)
	if b.status == 0 {
		b.status = 200
	}
	if idemID != "" {
		if b.status < 300 {
			if _, err = a.store.DB.Exec("UPDATE api_idempotency SET status_code=?,response=? WHERE id=?", b.status, b.body.String(), idemID); err != nil {
				apiError(w, 503, "result_unconfirmed", "操作可能已执行，但保存响应失败；请查询结果后再处理")
				return
			}
		} else if b.status < 500 {
			_, _ = a.store.DB.Exec("DELETE FROM api_idempotency WHERE id=?", idemID)
		}
	}
	if mutation {
		a.recordAPIAudit(c.ID, r.Method+" "+r.URL.Path, b.status, w.Header().Get("X-Request-ID"))
		a.invalidateStats()
	}
	if b.status >= 400 {
		message := "请求失败"
		var result struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b.body.Bytes(), &result) == nil && result.Error != "" {
			message = result.Error
		}
		apiError(w, b.status, externalErrorCode(b.status), message)
		return
	}
	for key, values := range b.Header() {
		if key != "X-Request-Id" {
			w.Header()[key] = values
		}
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}
func (a *App) externalMe(w http.ResponseWriter, r *http.Request, c apiCredential) { respond(w, 200, c) }

// Credential-protected bytes must be reauthorized, even for versioned URLs.
type externalMediaWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *externalMediaWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Authorization")
	w.ResponseWriter.WriteHeader(status)
}
func (w *externalMediaWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *externalMediaWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
