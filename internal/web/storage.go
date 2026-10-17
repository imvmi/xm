package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"xm/internal/archive"
)

// Only these fields can leave the server. Never embed the credential-bearing
// configuration in a JSON response, even when adding new provider fields.
type storageView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Endpoint    string `json:"endpoint"`
	Prefix      string `json:"prefix"`
	Username    string `json:"username"`
	Region      string `json:"region"`
	Bucket      string `json:"bucket"`
	PathStyle   bool   `json:"path_style"`
	UseProxy    bool   `json:"use_proxy"`
	Timeout     int    `json:"timeout_seconds"`
	PasswordSet bool   `json:"password_configured"`
	KeysSet     bool   `json:"keys_configured"`
	TokenSet    bool   `json:"token_configured"`
}

func (a *App) storageSettings(w http.ResponseWriter, r *http.Request) {
	cfg := a.Config()
	profiles := []storageView{}
	for _, p := range cfg.StorageProfiles {
		profiles = append(profiles, storageView{p.ID, p.Name, p.Kind, p.Endpoint, p.Prefix, p.Username, p.Region, p.Bucket, p.PathStyle, p.UseProxy, p.Timeout, p.Password != "", p.AccessKey != "" && p.SecretKey != "", p.SessionToken != ""})
	}
	respond(w, 200, map[string]any{"active": cfg.ActiveStorage(), "profiles": profiles})
}
func (a *App) saveStorageProfile(w http.ResponseWriter, r *http.Request) {
	var v struct {
		archive.StorageProfile
		ClearToken bool `json:"clear_token"`
	}
	if !decode(w, r, &v) {
		return
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	cfg := a.Config()
	p := v.StorageProfile
	p.Name = strings.TrimSpace(p.Name)
	p.Endpoint = strings.TrimRight(strings.TrimSpace(p.Endpoint), "/")
	p.Prefix = strings.Trim(strings.TrimSpace(p.Prefix), "/")
	index := -1
	if p.ID != "" {
		for i, old := range cfg.StorageProfiles {
			if old.ID == p.ID {
				index = i
				if !p.SameDestination(old) {
					problem(w, 400, "已有连接的存储地址不能修改，请新增连接以保留旧媒体访问")
					return
				}
				if p.Password == "" {
					p.Password = old.Password
				}
				if p.AccessKey == "" {
					p.AccessKey = old.AccessKey
				}
				if p.SecretKey == "" {
					p.SecretKey = old.SecretKey
				}
				if p.SessionToken == "" {
					p.SessionToken = old.SessionToken
				}
				break
			}
		}
		if index < 0 {
			problem(w, 404, "存储连接不存在")
			return
		}
	} else {
		if len(cfg.StorageProfiles) >= 32 {
			problem(w, 400, "最多保存 32 个存储连接")
			return
		}
		nonce := make([]byte, 12)
		if _, err := rand.Read(nonce); err != nil {
			problem(w, 500, "无法创建存储标识")
			return
		}
		p.ID = "store_" + hex.EncodeToString(nonce)
	}
	if v.ClearToken {
		p.SessionToken = ""
	}
	if err := p.Validate(); err != nil {
		problem(w, 400, err.Error())
		return
	}
	if index < 0 {
		cfg.StorageProfiles = append(cfg.StorageProfiles, p)
	} else {
		cfg.StorageProfiles[index] = p
	}
	if err := a.saveConfig(cfg); err != nil {
		problem(w, 500, "保存存储配置失败")
		return
	}
	respond(w, 200, map[string]string{"id": p.ID})
}
func (a *App) activateStorage(w http.ResponseWriter, r *http.Request) {
	var v struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &v) {
		return
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	cfg := a.Config()
	cfg.MediaStorage = v.ID
	if v.ID == "" {
		problem(w, 400, "请选择存储连接")
		return
	}
	if err := cfg.ValidateStorage(); err != nil {
		problem(w, 400, err.Error())
		return
	}
	if err := a.saveConfig(cfg); err != nil {
		problem(w, 500, "保存存储配置失败")
		return
	}
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) testStorage(w http.ResponseWriter, r *http.Request) {
	b, err := archive.NewRemote(a.Config(), r.PathValue("id"))
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	defer b.Close()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err = b.Test(ctx); err != nil {
		problem(w, 502, err.Error())
		return
	}
	respond(w, 200, map[string]string{"message": "写入、读取和删除测试通过"})
}
func (a *App) remoteMedia(w http.ResponseWriter, r *http.Request, id, key string, size int64) {
	ext := strings.ToLower(filepath.Ext(key))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif", ".mp4":
	default:
		http.NotFound(w, r)
		return
	}
	b, err := archive.NewRemote(a.Config(), id)
	if err != nil {
		problem(w, 502, err.Error())
		return
	}
	defer b.Close()
	n, err := b.Size(r.Context(), key)
	if err != nil || n != size {
		problem(w, 502, "远程媒体不可用或长度不一致，请检查存储连接")
		return
	}
	reader := &archive.RemoteReader{Remote: b, Context: r.Context(), Key: key, Length: size}
	defer reader.Close()
	if r.Method != http.MethodHead {
		if err = reader.Prepare(); err != nil {
			problem(w, 502, err.Error())
			return
		}
	}
	w.Header().Set("Content-Type", mime.TypeByExtension(ext))
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, filepath.Base(key), time.Time{}, reader)
}
