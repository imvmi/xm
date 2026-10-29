package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"xm/internal/archive"
)

type apiCredential struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	SecretHash   string   `json:"-"`
	SecretTail   string   `json:"secret_tail"`
	Scopes       []string `json:"scopes"`
	Accounts     []string `json:"accounts"`
	AllAccounts  bool     `json:"all_accounts"`
	Enabled      bool     `json:"enabled"`
	RevokedAt    int64    `json:"revoked_at"`
	ExpiresAt    int64    `json:"expires_at"`
	CreatedAt    int64    `json:"created_at"`
	LastUsedAt   int64    `json:"last_used_at"`
	RequestLimit int      `json:"request_limit"`
	TaskLimit    int      `json:"task_limit"`
	QueueLimit   int      `json:"queue_limit"`
}

type apiCredentialInput struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Scopes       []string `json:"scopes"`
	Accounts     []string `json:"accounts"`
	AllAccounts  bool     `json:"all_accounts"`
	Enabled      bool     `json:"enabled"`
	ExpiresAt    int64    `json:"expires_at"`
	RequestLimit int      `json:"request_limit"`
	TaskLimit    int      `json:"task_limit"`
	QueueLimit   int      `json:"queue_limit"`
}

const credentialColumns = "id,name,description,secret_hash,secret_tail,scopes,accounts,all_accounts,enabled,revoked_at,expires_at,created_at,last_used_at,request_limit,task_limit,queue_limit"

func scanAPICredential(row scanner) (apiCredential, error) {
	var c apiCredential
	var scopes, accounts string
	err := row.Scan(&c.ID, &c.Name, &c.Description, &c.SecretHash, &c.SecretTail, &scopes, &accounts, &c.AllAccounts, &c.Enabled, &c.RevokedAt, &c.ExpiresAt, &c.CreatedAt, &c.LastUsedAt, &c.RequestLimit, &c.TaskLimit, &c.QueueLimit)
	if err == nil {
		err = json.Unmarshal([]byte(scopes), &c.Scopes)
	}
	if err == nil {
		err = json.Unmarshal([]byte(accounts), &c.Accounts)
	}
	c.Scopes = expandAPIScopes(c.Scopes)
	return c, err
}
func (c apiCredential) allows(scope string) bool { return slices.Contains(c.Scopes, scope) }
func (c apiCredential) account(target string) bool {
	return c.AllAccounts || slices.Contains(c.Accounts, target)
}
func apiDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func (a *App) apiCredential(id string) (apiCredential, error) {
	return scanAPICredential(a.store.DB.QueryRow("SELECT "+credentialColumns+" FROM api_credentials WHERE id=?", id))
}
func (a *App) authenticateAPI(r *http.Request) (apiCredential, error) {
	header := r.Header.Get("Authorization")
	if len(header) > 256 || !strings.HasPrefix(header, "Bearer xm_") {
		return apiCredential{}, errors.New("invalid credential")
	}
	token := strings.TrimPrefix(header, "Bearer ")
	id, _, ok := strings.Cut(strings.TrimPrefix(token, "xm_"), ".")
	if !ok || len(id) != 26 {
		return apiCredential{}, errors.New("invalid credential")
	}
	c, err := scanAPICredential(a.store.DB.QueryRowContext(r.Context(), "SELECT "+credentialColumns+" FROM api_credentials WHERE id=?", id))
	if err != nil {
		return c, err
	}
	now := time.Now().Unix()
	if !c.Enabled || c.RevokedAt != 0 || (c.ExpiresAt != 0 && c.ExpiresAt <= now) || subtle.ConstantTimeCompare([]byte(c.SecretHash), []byte(apiDigest(token))) != 1 {
		return c, errors.New("invalid credential")
	}
	return c, nil
}

func (a *App) validateCredential(in *apiCredentialInput) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len([]rune(in.Name)) > 100 || len(in.Description) > 2000 {
		return errors.New("名称不能为空且最多 100 字，备注最多 2000 字节")
	}
	if len(in.Scopes) == 0 || len(in.Scopes) > len(apiScopes)+len(legacyAPIScopes) {
		return errors.New("请选择有效权限")
	}
	for _, s := range in.Scopes {
		if _, legacy := legacyAPIScopes[s]; !legacy && !slices.Contains(apiScopes, s) {
			return errors.New("包含未知权限")
		}
	}
	in.Scopes = expandAPIScopes(in.Scopes)
	if len(in.Scopes) == 0 {
		return errors.New("请选择有效权限")
	}
	if in.RequestLimit < 1 || in.RequestLimit > 600 || in.TaskLimit < 1 || in.TaskLimit > 60 || in.QueueLimit < 1 || in.QueueLimit > 100 {
		return errors.New("每分钟请求数为 1～600，任务提交数为 1～60，未结束任务数为 1～100")
	}
	if in.ExpiresAt < 0 || (in.ExpiresAt != 0 && in.ExpiresAt <= time.Now().Unix()) {
		return errors.New("有效期必须晚于当前时间，或设为永不过期")
	}
	if len(in.Accounts) > 100 || (!in.AllAccounts && len(in.Accounts) == 0) {
		return errors.New("请指定 1～100 个授权账号，或明确允许全部账号")
	}
	accounts := []string{}
	for _, target := range in.Accounts {
		name, err := archive.Username(target)
		if err != nil {
			return err
		}
		var canonical string
		err = a.store.DB.QueryRow("SELECT target FROM target_identities WHERE target=? OR current_username=? ORDER BY CASE WHEN target=? THEN 0 ELSE 1 END,target LIMIT 1", name, name, name).Scan(&canonical)
		if err == nil {
			name = canonical
		} else if !errors.Is(err, sql.ErrNoRows) {
			return errors.New("读取账号身份失败")
		}
		if !slices.Contains(accounts, name) {
			accounts = append(accounts, name)
		}
	}
	in.Accounts = accounts
	slices.Sort(in.Scopes)
	in.Scopes = slices.Compact(in.Scopes)
	return nil
}
func (a *App) listAPICredentials(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.DB.Query("SELECT " + credentialColumns + " FROM api_credentials ORDER BY CASE WHEN revoked_at=0 THEN 0 ELSE 1 END,created_at DESC,id LIMIT 200")
	if err != nil {
		problem(w, 500, "读取凭据失败")
		return
	}
	defer rows.Close()
	items := []apiCredential{}
	for rows.Next() {
		c, e := scanAPICredential(rows)
		if e != nil {
			problem(w, 500, "读取凭据失败")
			return
		}
		items = append(items, c)
	}
	if rows.Err() != nil {
		problem(w, 500, "读取凭据失败")
		return
	}
	respond(w, 200, map[string]any{"items": items, "scopes": apiScopes, "scope_definitions": apiScopeDefinitions})
}
func (a *App) saveAPICredential(w http.ResponseWriter, r *http.Request) {
	var in apiCredentialInput
	if !decode(w, r, &in) {
		return
	}
	if err := a.validateCredential(&in); err != nil {
		problem(w, 400, err.Error())
		return
	}
	a.api.writeMu.Lock()
	defer a.api.writeMu.Unlock()
	id := r.PathValue("id")
	created := id == ""
	secret := ""
	scopes, _ := json.Marshal(in.Scopes)
	accounts, _ := json.Marshal(in.Accounts)
	var err error
	if created {
		var count int
		if err = a.store.DB.QueryRow("SELECT COUNT(*) FROM api_credentials WHERE revoked_at=0").Scan(&count); err != nil {
			problem(w, 500, "读取凭据失败")
			return
		}
		if count >= 100 {
			problem(w, 409, "最多保存 100 个未撤销凭据")
			return
		}
		id = rand.Text()
		secret = "xm_" + id + "." + randomToken()
		_, err = a.store.DB.Exec("INSERT INTO api_credentials("+credentialColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", id, in.Name, in.Description, apiDigest(secret), secret[len(secret)-4:], string(scopes), string(accounts), in.AllAccounts, in.Enabled, 0, in.ExpiresAt, time.Now().Unix(), 0, in.RequestLimit, in.TaskLimit, in.QueueLimit)
	} else {
		c, e := a.apiCredential(id)
		if e != nil || c.RevokedAt != 0 {
			problem(w, 404, "凭据不存在或已撤销")
			return
		}
		_, err = a.store.DB.Exec("UPDATE api_credentials SET name=?,description=?,scopes=?,accounts=?,all_accounts=?,enabled=?,expires_at=?,request_limit=?,task_limit=?,queue_limit=? WHERE id=?", in.Name, in.Description, string(scopes), string(accounts), in.AllAccounts, in.Enabled, in.ExpiresAt, in.RequestLimit, in.TaskLimit, in.QueueLimit, id)
	}
	if err != nil {
		problem(w, 500, "保存凭据失败")
		return
	}
	a.recordAPIAudit("admin", r.Method+" /api/credentials/"+id, 200, rand.Text())
	c, err := a.apiCredential(id)
	if err != nil {
		problem(w, 500, "读取凭据失败")
		return
	}
	code := 200
	if created {
		code = 201
	}
	respond(w, code, map[string]any{"credential": c, "secret": secret})
}
func (a *App) revokeAPICredential(w http.ResponseWriter, r *http.Request) {
	a.api.writeMu.Lock()
	defer a.api.writeMu.Unlock()
	id := r.PathValue("id")
	if _, err := a.apiCredential(id); err != nil {
		problem(w, 404, "凭据不存在")
		return
	}
	if _, err := a.store.DB.Exec("UPDATE api_credentials SET enabled=0,revoked_at=? WHERE id=?", time.Now().Unix(), id); err != nil {
		problem(w, 500, "撤销凭据失败")
		return
	}
	a.recordAPIAudit("admin", "revoke "+id, 200, rand.Text())
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) rotateAPICredential(w http.ResponseWriter, r *http.Request) {
	a.api.writeMu.Lock()
	defer a.api.writeMu.Unlock()
	c, err := a.apiCredential(r.PathValue("id"))
	if err != nil || c.RevokedAt != 0 {
		problem(w, 404, "凭据不存在或已撤销")
		return
	}
	secret := "xm_" + c.ID + "." + randomToken()
	if _, err = a.store.DB.Exec("UPDATE api_credentials SET secret_hash=?,secret_tail=? WHERE id=?", apiDigest(secret), secret[len(secret)-4:], c.ID); err != nil {
		problem(w, 500, "轮换凭据失败")
		return
	}
	a.recordAPIAudit("admin", "rotate "+c.ID, 200, rand.Text())
	respond(w, 200, map[string]string{"secret": secret})
}
func (a *App) recordAPIAudit(credential, action string, status int, id string) {
	now := time.Now().Unix()
	// Bounded audit; no request bodies, query strings, or secrets are stored.
	_, err := a.store.DB.Exec("INSERT INTO api_audit(id,credential_id,action,status_code,created_at) VALUES(?,?,?,?,?)", id, credential, action, status, now)
	if err != nil {
		return
	}
	_, _ = a.store.DB.Exec("DELETE FROM api_audit WHERE created_at<? OR id NOT IN (SELECT id FROM (SELECT id FROM api_audit ORDER BY created_at DESC,id DESC LIMIT 1000) retained)", now-30*86400)
	_, _ = a.store.DB.Exec("DELETE FROM api_credentials WHERE revoked_at>0 AND revoked_at<?", now-30*86400)
}
func (a *App) listAPIAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.DB.Query("SELECT id,credential_id,action,status_code,created_at FROM api_audit ORDER BY created_at DESC,id DESC LIMIT 100")
	if err != nil {
		problem(w, 500, "读取审计记录失败")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, c, action string
		var status int
		var at int64
		if rows.Scan(&id, &c, &action, &status, &at) != nil {
			problem(w, 500, "读取审计记录失败")
			return
		}
		items = append(items, map[string]any{"id": id, "credential_id": c, "action": action, "status": status, "created_at": at})
	}
	if rows.Err() != nil {
		problem(w, 500, "读取审计记录失败")
		return
	}
	respond(w, 200, items)
}

type apiWindow struct {
	minute          int64
	requests, tasks int
}
type externalAPIState struct {
	writeMu sync.Mutex
	limitMu sync.Mutex
	windows map[string]apiWindow
}

func (a *App) allowAPIRequest(c apiCredential, task bool) bool {
	a.api.limitMu.Lock()
	defer a.api.limitMu.Unlock()
	minute := time.Now().Unix() / 60
	for id, v := range a.api.windows {
		if v.minute != minute {
			delete(a.api.windows, id)
		}
	}
	v := a.api.windows[c.ID]
	v.minute = minute
	if v.requests >= c.RequestLimit || (task && v.tasks >= c.TaskLimit) {
		return false
	}
	v.requests++
	if task {
		v.tasks++
	}
	a.api.windows[c.ID] = v
	return true
}
