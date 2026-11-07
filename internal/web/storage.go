package web

import (
	"net/http"
)

// Only these fields can leave the server. Never embed the credential-bearing
// configuration in a JSON response, even when adding new provider fields.
type storageView struct {
	CanDelete    bool   `json:"can_delete"`
	DeleteReason string `json:"delete_reason"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Endpoint     string `json:"endpoint"`
	Prefix       string `json:"prefix"`
	Username     string `json:"username"`
	Region       string `json:"region"`
	Bucket       string `json:"bucket"`
	PathStyle    bool   `json:"path_style"`
	UseProxy     bool   `json:"use_proxy"`
	Timeout      int    `json:"timeout_seconds"`
	PasswordSet  bool   `json:"password_configured"`
	KeysSet      bool   `json:"keys_configured"`
	TokenSet     bool   `json:"token_configured"`
}

func (a *App) storageSettings(w http.ResponseWriter, r *http.Request) {
	cfg := a.Config()
	profiles := []storageView{}
	job := a.migrations.snapshot()
	busy := a.maintenance.Load() || (job != nil && (job.Status == "running" || job.Status == "switching"))
	for _, p := range cfg.StorageProfiles {
		media, copies, err := storageReferenceCounts(r.Context(), a.store.DB, p.ID)
		if err != nil {
			problem(w, 500, "无法检查存储引用，请稍后重试")
			return
		}
		reason := storageDeletionReason(cfg, p.ID, media, copies, job, busy)
		profiles = append(profiles, storageView{CanDelete: reason == "", DeleteReason: reason, ID: p.ID, Name: p.Name, Kind: p.Kind, Endpoint: p.Endpoint, Prefix: p.Prefix, Username: p.Username, Region: p.Region, Bucket: p.Bucket, PathStyle: p.PathStyle, UseProxy: p.UseProxy, Timeout: p.Timeout, PasswordSet: p.Password != "", KeysSet: p.AccessKey != "" && p.SecretKey != "", TokenSet: p.SessionToken != ""})
	}
	cache := a.remoteMediaCacheSettings()
	used, _ := remoteMediaCacheUsage(cfg.DataDir)
	respond(w, 200, map[string]any{"active": cfg.ActiveStorage(), "profiles": profiles, "locked": true, "cache": map[string]any{"enabled": cache.Enabled, "max_bytes": cache.MaxBytes, "used_bytes": used}})
}
