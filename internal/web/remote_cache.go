package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"xm/internal/archive"
)

const (
	remoteMediaCacheStateKey = "remote_media_cache"
	defaultRemoteCacheBytes  = int64(2 << 30)
	minRemoteCacheBytes      = int64(64 << 20)
	maxRemoteCacheBytes      = int64(100 << 30)
)

type remoteMediaCacheSettings struct {
	Enabled  bool  `json:"enabled"`
	MaxBytes int64 `json:"max_bytes"`
}

type cacheEntryLock struct {
	mu   sync.Mutex
	refs int
}

func (a *App) remoteMediaCacheSettings() remoteMediaCacheSettings {
	settings := remoteMediaCacheSettings{MaxBytes: defaultRemoteCacheBytes}
	raw := state(a.store.DB, remoteMediaCacheStateKey)
	if raw == "" {
		return settings
	}
	var saved remoteMediaCacheSettings
	if json.Unmarshal([]byte(raw), &saved) != nil || saved.MaxBytes < minRemoteCacheBytes || saved.MaxBytes > maxRemoteCacheBytes {
		return settings
	}
	return saved
}

func (a *App) updateRemoteMediaCache(w http.ResponseWriter, r *http.Request) {
	var input remoteMediaCacheSettings
	if !decode(w, r, &input) {
		return
	}
	if input.MaxBytes < minRemoteCacheBytes || input.MaxBytes > maxRemoteCacheBytes {
		problem(w, 400, "缓存容量必须为 64 MiB 到 100 GiB")
		return
	}
	raw, err := json.Marshal(input)
	if err != nil || setState(a.store.DB, remoteMediaCacheStateKey, string(raw)) != nil {
		problem(w, 500, "保存缓存设置失败")
		return
	}
	if err = a.pruneRemoteMediaCache(input.MaxBytes, ""); err != nil {
		problem(w, 500, "设置已保存，但清理超额缓存失败")
		return
	}
	used, _ := remoteMediaCacheUsage(a.Config().DataDir)
	respond(w, 200, map[string]any{"enabled": input.Enabled, "max_bytes": input.MaxBytes, "used_bytes": used})
}

func (a *App) clearRemoteMediaCache(w http.ResponseWriter, _ *http.Request) {
	a.cachePruneMu.Lock()
	err := os.RemoveAll(remoteMediaCacheRoot(a.Config().DataDir))
	a.cachePruneMu.Unlock()
	if err != nil {
		problem(w, 500, "清空本地缓存失败")
		return
	}
	respond(w, 200, map[string]any{"ok": true, "used_bytes": 0})
}

func remoteMediaCacheRoot(dataDir string) string {
	return filepath.Join(dataDir, "cache", "remote-media")
}

func remoteMediaCachePath(dataDir, storageID, key, hash, ext string, size int64) string {
	sum := sha256.Sum256([]byte(storageID + "\x00" + key + "\x00" + hash + "\x00" + strconv.FormatInt(size, 10)))
	return filepath.Join(remoteMediaCacheRoot(dataDir), hex.EncodeToString(sum[:])+ext)
}

func validRemoteMediaCache(path string, size int64) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		if err == nil {
			_ = os.Remove(path)
		}
		return false
	}
	return true
}

func (a *App) lockCacheEntry(path string) func() {
	a.cacheLockMu.Lock()
	if a.cacheLocks == nil {
		a.cacheLocks = map[string]*cacheEntryLock{}
	}
	entry := a.cacheLocks[path]
	if entry == nil {
		entry = &cacheEntryLock{}
		a.cacheLocks[path] = entry
	}
	entry.refs++
	a.cacheLockMu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		a.cacheLockMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(a.cacheLocks, path)
		}
		a.cacheLockMu.Unlock()
	}
}

func (a *App) ensureRemoteMediaCached(ctx context.Context, id, key string, size int64, expectedHash, ext string, maxBytes int64) (string, error) {
	path := remoteMediaCachePath(a.Config().DataDir, id, key, expectedHash, ext, size)
	if validRemoteMediaCache(path, size) {
		return path, nil
	}
	unlock := a.lockCacheEntry(path)
	defer unlock()
	if validRemoteMediaCache(path, size) {
		return path, nil
	}

	remote, err := archive.NewRemote(a.Config(), id)
	if err != nil {
		return "", err
	}
	defer remote.Close()
	remoteSize, err := remote.Size(ctx, key)
	if err != nil || remoteSize != size {
		return "", errors.New("远程媒体不可用或长度不一致，请检查存储连接")
	}
	body, err := remote.Open(ctx, key, 0)
	if err != nil {
		return "", err
	}
	defer body.Close()

	root := remoteMediaCacheRoot(a.Config().DataDir)
	if err = archive.PrivateDir(root); err != nil {
		return "", errors.New("无法创建远程媒体缓存目录")
	}
	temp, err := os.CreateTemp(root, ".cache-*")
	if err != nil {
		return "", errors.New("无法创建远程媒体缓存文件")
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err = temp.Chmod(0600); err != nil {
		temp.Close()
		return "", err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temp, hasher), io.LimitReader(body, size+1))
	if copyErr == nil && written != size {
		copyErr = errors.New("远程媒体长度不一致")
	}
	if copyErr == nil && len(expectedHash) == 64 && !strings.EqualFold(hex.EncodeToString(hasher.Sum(nil)), expectedHash) {
		copyErr = errors.New("远程媒体校验失败")
	}
	if copyErr == nil {
		copyErr = temp.Sync()
	}
	if closeErr := temp.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return "", copyErr
	}
	if err = os.Rename(tempName, path); err != nil {
		return "", errors.New("无法提交远程媒体缓存文件")
	}
	if err = a.pruneRemoteMediaCache(maxBytes, path); err != nil {
		return "", err
	}
	return path, nil
}

func serveRemoteCacheFile(w http.ResponseWriter, r *http.Request, path, key, ext string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	w.Header().Set("Content-Type", mime.TypeByExtension(ext))
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-XM-Media-Cache", "hit")
	http.ServeContent(w, r, filepath.Base(key), info.ModTime(), f)
	return true
}

func (a *App) remoteMedia(w http.ResponseWriter, r *http.Request, id, key string, size int64, hash string) {
	ext := strings.ToLower(filepath.Ext(key))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif", ".mp4":
	default:
		http.NotFound(w, r)
		return
	}
	cache := a.remoteMediaCacheSettings()
	if cache.Enabled && r.Method == http.MethodGet && size <= cache.MaxBytes {
		path, err := a.ensureRemoteMediaCached(r.Context(), id, key, size, hash, ext, cache.MaxBytes)
		if err == nil && serveRemoteCacheFile(w, r, path, key, ext) {
			return
		}
	}
	a.streamRemoteMedia(w, r, id, key, size, ext)
}

func (a *App) streamRemoteMedia(w http.ResponseWriter, r *http.Request, id, key string, size int64, ext string) {
	remote, err := archive.NewRemote(a.Config(), id)
	if err != nil {
		problem(w, 502, err.Error())
		return
	}
	defer remote.Close()
	n, err := remote.Size(r.Context(), key)
	if err != nil || n != size {
		problem(w, 502, "远程媒体不可用或长度不一致，请检查存储连接")
		return
	}
	reader := &archive.RemoteReader{Remote: remote, Context: r.Context(), Key: key, Length: size}
	defer reader.Close()
	if r.Method != http.MethodHead {
		if err = reader.Prepare(); err != nil {
			problem(w, 502, err.Error())
			return
		}
	}
	w.Header().Set("Content-Type", mime.TypeByExtension(ext))
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("X-XM-Media-Cache", "miss")
	http.ServeContent(w, r, filepath.Base(key), time.Time{}, reader)
}

type cachedFile struct {
	path    string
	size    int64
	modTime time.Time
}

func remoteMediaCacheUsage(dataDir string) (int64, error) {
	entries, err := os.ReadDir(remoteMediaCacheRoot(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total, nil
}

func (a *App) pruneRemoteMediaCache(maxBytes int64, keep string) error {
	a.cachePruneMu.Lock()
	defer a.cachePruneMu.Unlock()
	root := remoteMediaCacheRoot(a.Config().DataDir)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	files := make([]cachedFile, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() {
			continue
		}
		file := cachedFile{path: filepath.Join(root, entry.Name()), size: info.Size(), modTime: info.ModTime()}
		files = append(files, file)
		total += file.size
	}
	if total <= maxBytes {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.Before(files[j].modTime) })
	for _, file := range files {
		if total <= maxBytes {
			break
		}
		if file.path == keep {
			continue
		}
		if err = os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("删除过期缓存: %w", err)
		}
		total -= file.size
	}
	return nil
}
