package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"xm/internal/archive"
)

// Omitted credentials are preserved; an explicit empty session token clears it.
// No destination or network fields are accepted by this endpoint.
type storageCredentialsInput struct {
	Username     *string `json:"username"`
	Password     *string `json:"password"`
	AccessKey    *string `json:"access_key"`
	SecretKey    *string `json:"secret_key"`
	SessionToken *string `json:"session_token"`
}

func (in storageCredentialsInput) apply(p archive.StorageProfile) (archive.StorageProfile, error) {
	if in.Username == nil && in.Password == nil && in.AccessKey == nil && in.SecretKey == nil && in.SessionToken == nil {
		return p, errors.New("请填写需要更新的凭据")
	}
	if p.Kind == "s3" {
		if in.Username != nil || in.Password != nil || (in.AccessKey == nil) != (in.SecretKey == nil) {
			return p, errors.New("S3 请同时填写 Access Key 和 Secret Key，或单独更新 Session Token")
		}
	} else if in.AccessKey != nil || in.SecretKey != nil || in.SessionToken != nil {
		return p, errors.New("此存储仅支持更新用户名和密码")
	}
	for _, pair := range []struct {
		value *string
		dest  *string
	}{
		{in.Username, &p.Username}, {in.Password, &p.Password}, {in.AccessKey, &p.AccessKey}, {in.SecretKey, &p.SecretKey}, {in.SessionToken, &p.SessionToken},
	} {
		if pair.value != nil {
			if len(*pair.value) > 16384 {
				return p, errors.New("凭据内容过长")
			}
			*pair.dest = *pair.value
		}
	}
	return p, p.Validate()
}

func (a *App) rotateStorageCredentials(w http.ResponseWriter, r *http.Request) {
	var in storageCredentialsInput
	if !decode(w, r, &in) {
		return
	}
	cfg := a.Config()
	id := r.PathValue("id")
	old, err := cfg.Storage(id)
	if err != nil {
		problem(w, 404, "存储连接不存在")
		return
	}
	p, err := in.apply(old)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	for i := range cfg.StorageProfiles {
		if cfg.StorageProfiles[i].ID == id {
			cfg.StorageProfiles[i] = p
		}
	}
	// Probe outside changeMu so a slow endpoint does not block scheduler controls.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	remote, err := archive.NewRemote(cfg, id)
	if err == nil {
		defer remote.Close()
		err = remote.Test(ctx)
	}
	if err != nil {
		problem(w, 502, "新凭据验证失败，请检查凭据及存储读写、清理权限；原配置保留")
		return
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	latest := a.Config()
	current, err := latest.Storage(id)
	if err != nil || current != old || latest.Proxy != cfg.Proxy {
		problem(w, 409, "验证期间存储连接或代理已变更，请重新提交")
		return
	}
	// Update only credential columns. Publishing memory follows a successful DB
	// write; other settings saved while the probe ran are never overwritten.
	result, err := a.store.DB.ExecContext(ctx, "UPDATE storage_profiles SET username=?,password=?,access_key=?,secret_key=?,session_token=?,updated_at=? WHERE id=?", p.Username, p.Password, p.AccessKey, p.SecretKey, p.SessionToken, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err == nil {
		var affected int64
		affected, err = result.RowsAffected()
		if affected != 1 {
			err = errors.New("存储连接不存在")
		}
	}
	if err != nil {
		problem(w, 500, "保存存储凭据失败，请重试；原运行配置保留")
		return
	}
	a.configMu.Lock()
	for i := range a.config.StorageProfiles {
		if a.config.StorageProfiles[i].ID == id {
			a.config.StorageProfiles[i] = p
		}
	}
	a.configMu.Unlock()
	respond(w, 200, map[string]string{"message": "凭据已验证并更新，新请求和下一次采集使用新凭据"})
}
