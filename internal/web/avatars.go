package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xm/internal/archive"
)

// Batch source lookups: a timeline may contain hundreds of distinct authors,
// and a remote database must not receive one round trip per missing cache file.
func (a *App) avatarURLs(ctx context.Context, ids []string) map[string]string {
	urls := map[string]string{}
	seen := map[string]bool{}
	missing := []any{}
	dataDir := a.Config().DataDir
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, err := archive.AvatarFilename(id); err != nil {
			continue
		}
		if archive.AvatarExists(dataDir, id) {
			urls[id] = "/avatars/" + id
		} else {
			missing = append(missing, id)
		}
	}
	for start := 0; start < len(missing); start += 500 {
		chunk := missing[start:min(start+500, len(missing))]
		rows, err := a.store.DB.QueryContext(ctx, "SELECT user_id FROM avatar_sources WHERE user_id IN ("+strings.TrimRight(strings.Repeat("?,", len(chunk)), ",")+")", chunk...)
		if err != nil {
			continue
		}
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				urls[id] = "/avatars/" + id
			}
		}
		rows.Close()
	}
	return urls
}

func openAvatarFile(dir, id string) (*os.File, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	st, err := root.Lstat(id)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 5<<20 {
		return nil, os.ErrNotExist
	}
	f, err := root.Open(id)
	if err != nil {
		return nil, err
	}
	var header [512]byte
	n, err := f.Read(header[:])
	if err != nil && !errors.Is(err, io.EOF) {
		f.Close()
		return nil, err
	}
	switch http.DetectContentType(header[:n]) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		f.Close()
		return nil, os.ErrNotExist
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (a *App) openAvatar(r *http.Request, id string) (*os.File, error) {
	cfg := a.Config()
	cacheDir := archive.AvatarCacheDir(cfg.DataDir)
	legacy := func() (*os.File, error) { return openAvatarFile(filepath.Join(cfg.DataDir, "avatars"), id) }
	if f, err := openAvatarFile(cacheDir, id); err == nil {
		return f, nil
	}
	source := a.store.AvatarSource(id)
	if source == "" {
		return legacy()
	}
	// Serialize misses and avoid new cache writes during database migration.
	if !a.writeGate.TryRLock() {
		return legacy()
	}
	defer a.writeGate.RUnlock()
	if a.maintenance.Load() {
		return legacy()
	}
	unlock := a.lockCacheEntry(filepath.Join(cacheDir, id))
	defer unlock()
	if f, err := openAvatarFile(cacheDir, id); err == nil {
		return f, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := a.scheduler.avatar(ctx, cfg, archive.Profile{ID: id, AvatarURL: source}); err != nil {
		return legacy()
	}
	if f, err := openAvatarFile(cacheDir, id); err == nil {
		return f, nil
	}
	return legacy()
}

type avatarCacheStatus struct {
	Sources   int   `json:"sources"`
	Cached    int   `json:"cached"`
	UsedBytes int64 `json:"used_bytes"`
	Legacy    int   `json:"legacy"`
}

func (a *App) avatarSettings(w http.ResponseWriter, r *http.Request) {
	var result avatarCacheStatus
	if err := a.store.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM avatar_sources").Scan(&result.Sources); err != nil {
		problem(w, 500, "读取头像来源失败")
		return
	}
	for _, dir := range []string{archive.AvatarCacheDir(a.Config().DataDir), filepath.Join(a.Config().DataDir, "avatars")} {
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			problem(w, 500, "读取头像目录失败")
			return
		}
		for _, entry := range entries {
			id := strings.TrimSuffix(entry.Name(), ".json")
			if _, err := archive.AvatarFilename(id); err != nil {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if dir == archive.AvatarCacheDir(a.Config().DataDir) {
				result.UsedBytes += info.Size()
				if entry.Name() == id {
					result.Cached++
				}
			} else if entry.Name() == id {
				result.Legacy++
			}
		}
	}
	respond(w, 200, result)
}
func (a *App) migrateAvatars(w http.ResponseWriter, r *http.Request) {
	result, err := a.store.MigrateLegacyAvatars(r.Context(), a.Config().DataDir)
	if err != nil {
		problem(w, 500, "读取旧头像失败，请检查数据目录权限后重试")
		return
	}
	respond(w, 200, result)
}
func (a *App) clearAvatars(w http.ResponseWriter, r *http.Request) {
	cleared, err := a.store.ClearAvatarCache(r.Context(), a.Config().DataDir)
	if err != nil {
		problem(w, 500, "部分头像缓存未清除，请稍后重试；来源 URL 和旧头像仍保留")
		return
	}
	respond(w, 200, map[string]any{"cleared": cleared})
}
