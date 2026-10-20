package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type LocalMediaCopy struct {
	CopyID string
	MigrationMedia
}

func (s *Store) LocalMediaCopies(ctx context.Context) ([]LocalMediaCopy, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT copy_id,post_id,media_id,local_path,storage_id,object_key,bytes,sha256 FROM media_local_copies ORDER BY copy_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []LocalMediaCopy
	for rows.Next() {
		var item LocalMediaCopy
		if err := rows.Scan(&item.CopyID, &item.PostID, &item.MediaID, &item.LocalPath, &item.StorageID, &item.Key, &item.Bytes, &item.SHA256); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// CleanupLocalMediaCopy must run with application writers excluded. It only
// deletes a recorded migrated copy whose current remote reference and contents
// are verified again; it never discovers candidates by walking the media tree.
func (s *Store) CleanupLocalMediaCopy(ctx context.Context, cfg Config, item LocalMediaCopy) error {
	if !safeObjectKey(item.LocalPath) || !strings.HasPrefix(item.LocalPath, "media/") || item.StorageID == "local" || item.Bytes <= 0 || len(item.SHA256) != 64 {
		return errors.New("本地副本记录无效，未删除")
	}
	if _, err := hex.DecodeString(item.SHA256); err != nil {
		return errors.New("本地副本校验值无效")
	}
	var matching int
	matchSQL := `SELECT COUNT(*) FROM media WHERE post_id=? AND media_id=? AND storage_id=? AND object_key=? AND local_path='' AND status='done' AND bytes=? AND sha256=?`
	args := []any{item.PostID, item.MediaID, item.StorageID, item.Key, item.Bytes, item.SHA256}
	if err := s.DB.QueryRowContext(ctx, matchSQL, args...).Scan(&matching); err != nil || matching != 1 {
		return errors.New("归档引用已变化，本地副本保留")
	}
	var referenced int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM media WHERE local_path=?", item.LocalPath).Scan(&referenced); err != nil || referenced != 0 {
		return errors.New("本地文件仍被归档引用，不能清理")
	}
	verify := func(r io.Reader) bool {
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(r, item.Bytes+1))
		return err == nil && n == item.Bytes && hex.EncodeToString(h.Sum(nil)) == item.SHA256
	}
	remote, err := NewRemote(cfg, item.StorageID)
	if err != nil {
		return err
	}
	defer remote.Close()
	r, err := remote.Open(ctx, item.Key, 0)
	if err != nil {
		return errors.New("无法读取远端文件，本地副本保留")
	}
	valid := verify(r)
	closeErr := r.Close()
	if !valid || closeErr != nil {
		return errors.New("远端文件回读校验失败，本地副本保留")
	}
	mediaRoot := filepath.Join(cfg.DataDir, "media")
	mediaInfo, err := os.Lstat(mediaRoot)
	if err != nil || !mediaInfo.IsDir() {
		return errors.New("本地媒体根目录无效，未删除")
	}
	root, err := os.OpenRoot(mediaRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	info, err := root.Lstat(strings.TrimPrefix(item.LocalPath, "media/"))
	missing := errors.Is(err, os.ErrNotExist)
	if !missing {
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("本地副本不是普通文件或无法读取，未删除")
		}
		f, err := root.Open(strings.TrimPrefix(item.LocalPath, "media/"))
		if err != nil {
			return errors.New("无法打开本地副本")
		}
		opened, statErr := f.Stat()
		valid = statErr == nil && os.SameFile(info, opened) && verify(f)
		f.Close()
		if !valid {
			return errors.New("本地副本内容已变化，未删除")
		}
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, matchSQL, args...).Scan(&matching); err != nil || matching != 1 {
		return errors.New("归档引用已变化，本地副本保留")
	}
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM media WHERE local_path=?", item.LocalPath).Scan(&referenced); err != nil || referenced != 0 {
		return errors.New("本地文件仍被归档引用，不能清理")
	}
	if !missing {
		current, err := root.Lstat(strings.TrimPrefix(item.LocalPath, "media/"))
		if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
			return errors.New("本地文件在校验后发生变化，未删除")
		}
		if err = root.Remove(strings.TrimPrefix(item.LocalPath, "media/")); err != nil {
			return errors.New("无法删除已校验的本地副本")
		}
	}
	// If commit fails after deletion, the retained record can safely be retried.
	if _, err = tx.ExecContext(ctx, "DELETE FROM media_local_copies WHERE copy_id=?", item.CopyID); err != nil {
		return err
	}
	return tx.Commit()
}
