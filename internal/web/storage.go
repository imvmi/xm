package web

import (
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
	respond(w, 200, map[string]any{"active": cfg.ActiveStorage(), "profiles": profiles, "locked": true})
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
