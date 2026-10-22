package archive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"xm/internal/database"
)

// A bounded lock set serializes refresh, legacy import and cache clearing.
var avatarLocks [64]sync.Mutex

func lockAvatar(id string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	m := &avatarLocks[h.Sum32()%uint32(len(avatarLocks))]
	m.Lock()
	return m.Unlock
}

func AvatarCacheDir(dataDir string) string { return filepath.Join(dataDir, "cache", "avatars") }
func cachedAvatarExists(dataDir, id string) bool {
	root, err := os.OpenRoot(AvatarCacheDir(dataDir))
	if err != nil {
		return false
	}
	defer root.Close()
	data, err := readAvatarFile(root, id, maxAvatarBytes)
	return err == nil && validAvatarImage(data)
}

func saveAvatarSource(tx *database.Tx, p Profile) error {
	if !userIDPattern.MatchString(p.ID) {
		return nil
	}
	if _, err := avatarURL(p.AvatarURL); err != nil {
		return nil
	}
	_, err := tx.Exec(database.Upsert(tx.Dialect, "INSERT INTO avatar_sources(user_id,source_url,updated_at) VALUES(?,?,?)", []string{"user_id"}, []string{"source_url", "updated_at"}), p.ID, p.AvatarURL, now())
	return err
}

func (s *Store) AvatarSource(id string) string {
	if !userIDPattern.MatchString(id) {
		return ""
	}
	var source string
	if s.DB.QueryRow("SELECT source_url FROM avatar_sources WHERE user_id=?", id).Scan(&source) != nil {
		return ""
	}
	if _, err := avatarURL(source); err != nil {
		return ""
	}
	return source
}

// Legacy files without a recoverable origin are deliberately left in avatars/.
// A missing cache is disposable only once its origin is durable in the DB.
type AvatarMigrationResult struct {
	Imported int `json:"imported"`
	Migrated int `json:"migrated"`
	Retained int `json:"retained"`
	Failed   int `json:"failed"`
}

func readAvatarFile(root *os.Root, name string, limit int64) ([]byte, error) {
	st, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("头像文件无效或过大")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("头像文件过大")
	}
	return data, err
}
func validAvatarImage(b []byte) bool {
	switch http.DetectContentType(b) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

func (s *Store) MigrateLegacyAvatars(ctx context.Context, dataDir string) (AvatarMigrationResult, error) {
	var result AvatarMigrationResult
	dir := filepath.Join(dataDir, "avatars")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return result, err
	}
	defer root.Close()
	seen := map[string]bool{}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !userIDPattern.MatchString(id) || seen[id] {
			continue
		}
		seen[id] = true
		unlock := lockAvatar(id)
		err = s.migrateLegacyAvatar(ctx, dataDir, root, id, &result)
		unlock()
		if err != nil {
			result.Failed++
		}
	}
	return result, nil
}

func (s *Store) migrateLegacyAvatar(ctx context.Context, dataDir string, root *os.Root, id string, result *AvatarMigrationResult) error {
	var meta avatarMetadata
	if b, err := readAvatarFile(root, id+".json", 64<<10); err == nil {
		_ = json.Unmarshal(b, &meta)
	}
	if _, err := avatarURL(meta.SourceURL); err == nil {
		// Never replace a newer source captured by the collector.
		res, err := s.DB.InsertIgnore("INSERT INTO avatar_sources(user_id,source_url,updated_at) VALUES(?,?,?)", "user_id", id, meta.SourceURL, now())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			result.Imported++
		}
	}
	var source string
	err := s.DB.QueryRowContext(ctx, "SELECT source_url FROM avatar_sources WHERE user_id=?", id).Scan(&source)
	if errors.Is(err, sql.ErrNoRows) {
		result.Retained++
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := avatarURL(source); err != nil {
		result.Retained++
		return nil
	}
	b, err := readAvatarFile(root, id, maxAvatarBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} // Metadata alone still restores the origin.
	if err != nil || !validAvatarImage(b) {
		return errors.New("旧头像不可读取")
	}
	if !cachedAvatarExists(dataDir, id) {
		// If the old file has no metadata, preserve it as a fallback until collection
		// refreshes it. Never label stale bytes as a newer known origin.
		if meta.SourceURL != "" && meta.SourceURL != source {
			result.Retained++
			return nil
		}
		dir := AvatarCacheDir(dataDir)
		if err = PrivateDir(dir); err != nil {
			return err
		}
		if err = writePrivateFileAtomic(filepath.Join(dir, id), b); err != nil {
			return err
		}
		if meta.SourceURL == source {
			if err = writeAvatarMetadata(filepath.Join(dir, id+".json"), meta); err != nil {
				return err
			}
		}
	}
	// The DB URL and cache file are committed before touching the old files.
	if err = root.Remove(id); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = root.Remove(id + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	result.Migrated++
	return nil
}

func (s *Store) ClearAvatarCache(ctx context.Context, dataDir string) (int, error) {
	entries, err := os.ReadDir(AvatarCacheDir(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	root, err := os.OpenRoot(AvatarCacheDir(dataDir))
	if err != nil {
		return 0, err
	}
	defer root.Close()
	cleared := 0
	seen := map[string]bool{}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return cleared, err
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !userIDPattern.MatchString(id) || seen[id] {
			continue
		}
		seen[id] = true
		unlock := lockAvatar(id)
		if s.AvatarSource(id) != "" {
			err = root.Remove(id)
			if err == nil {
				cleared++
			}
			if err == nil || errors.Is(err, os.ErrNotExist) {
				err = root.Remove(id + ".json")
			}
		}
		unlock()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return cleared, err
		}
	}
	return cleared, nil
}
