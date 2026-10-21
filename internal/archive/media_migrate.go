package archive

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"

	"xm/internal/database"
)

type MigrationMedia struct {
	PostID    string `json:"post_id"`
	MediaID   string `json:"media_id"`
	StorageID string `json:"-"`
	Key       string `json:"-"`
	LocalPath string `json:"-"`
	Bytes     int64  `json:"-"`
	SHA256    string `json:"-"`
}

// LocalizeMedia changes the pointer only after a durable, verified local copy.
// Source objects and download attempt counters are never changed or deleted.
func (s *Store) LocalizeMedia(ctx context.Context, cfg Config, item MigrationMedia) error {
	profile, storageErr := cfg.Storage(item.StorageID)
	if storageErr != nil || (profile.Kind != "s3" && profile.Kind != "webdav" && profile.Kind != "smb") {
		return errors.New("源远端存储配置不可用")
	}
	if item.Bytes <= 0 || len(item.SHA256) != 64 {
		return errors.New("缺少有效大小或 SHA256，无法安全迁移")
	}
	if _, err := hex.DecodeString(item.SHA256); err != nil {
		return errors.New("SHA256 格式无效")
	}
	ext := strings.ToLower(path.Ext(item.Key))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".webp", ".gif", ".avif", ".mp4":
	default:
		return errors.New("媒体扩展名不受支持")
	}
	rel, err := s.migrationMediaPath(ctx, item, ext)
	if err != nil {
		return err
	}
	directory := path.Dir(rel)
	root, err := os.OpenRoot(cfg.DataDir)
	if err != nil {
		return errors.New("无法打开本地数据目录")
	}
	defer root.Close()
	if err = root.MkdirAll(directory, 0700); err != nil {
		return errors.New("无法创建本地媒体目录")
	}
	verify := func(f *os.File) bool {
		h := sha256.New()
		n, e := io.Copy(h, io.LimitReader(f, item.Bytes+1))
		return e == nil && n == item.Bytes && hex.EncodeToString(h.Sum(nil)) == item.SHA256
	}
	// Windows FlushFileBuffers requires a writable handle, including retries.
	if f, e := root.OpenFile(rel, os.O_RDWR, 0); e == nil {
		valid := verify(f)
		syncErr := f.Sync()
		f.Close()
		if !valid {
			return errors.New("目标文件已存在但校验不符，请检查本地磁盘")
		}
		if syncErr != nil {
			return errors.New("本地文件同步失败")
		}
	} else {
		if !os.IsNotExist(e) {
			return errors.New("无法读取本地目标文件")
		}
		remote, e := NewRemote(cfg, item.StorageID)
		if e != nil {
			return errors.New("无法创建远端存储连接")
		}
		defer remote.Close()
		r, e := remote.Open(ctx, item.Key, 0)
		if e != nil {
			return errors.New("远端读取失败，请检查网络、权限和源文件后重试")
		}
		defer r.Close()
		tmp := directory + "/.migration-" + rand.Text()
		f, e := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if e != nil {
			return errors.New("无法创建本地临时文件")
		}
		defer root.Remove(tmp)
		defer f.Close()
		h := sha256.New()
		n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, item.Bytes+1))
		if e != nil {
			return errors.New("远端下载或本地写入失败，请重试")
		}
		if n != item.Bytes || hex.EncodeToString(h.Sum(nil)) != item.SHA256 {
			return errors.New("文件大小或 SHA256 校验失败，保留远端引用")
		}
		if e = f.Sync(); e != nil {
			return errors.New("文件同步失败，请检查磁盘空间")
		}
		if _, e = f.Seek(0, io.SeekStart); e != nil || !verify(f) {
			return errors.New("本地文件回读校验失败")
		}
		if e = f.Close(); e != nil {
			return errors.New("关闭本地文件失败")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Publish without overwriting a destination created since our check.
		if e = publishMigrationFile(root, tmp, rel); e != nil {
			return errors.New("本地文件提交失败")
		}
	}
	// Also sync on a retry that found an already verified file: a previous
	// attempt may have stopped between file publication, directory sync and DB commit.
	for dir := directory; ; dir = path.Dir(dir) {
		d, e := root.Open(dir)
		if e != nil {
			return errors.New("本地目录同步失败")
		}
		e = SyncDirectory(d)
		d.Close()
		if e != nil {
			return errors.New("本地目录同步失败")
		}
		if dir == "." {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE media SET storage_id='local',local_path=?,object_key='',updated_at=? WHERE post_id=? AND media_id=? AND status='done' AND storage_id=? AND object_key=? AND bytes=? AND sha256=?`, rel, now(), item.PostID, item.MediaID, item.StorageID, item.Key, item.Bytes, item.SHA256)
	if err != nil {
		return errors.New("本地副本已验证，但更新媒体索引失败；可安全重试")
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return errors.New("媒体索引已变化，未覆盖；请重试")
	}
	return nil
}

func (s *Store) migrationMediaPath(ctx context.Context, item MigrationMedia, ext string) (string, error) {
	job := MediaJob{PostID: item.PostID, ID: item.MediaID}
	if err := s.DB.QueryRowContext(ctx, "SELECT author_id,username FROM posts WHERE id=?", item.PostID).Scan(&job.AuthorID, &job.Username); err != nil {
		return "", errors.New("无法读取媒体作者，未修改源文件")
	}
	var err error
	job.Directory, err = s.MediaDirectory(job)
	if err != nil {
		return "", err
	}
	return mediaRelativePath(job, ext)
}

// RemoteizeMedia retains the local source and switches the index only after
// reading back and hashing the uploaded object. Author paths are preserved.
func (s *Store) RemoteizeMedia(ctx context.Context, cfg Config, item MigrationMedia, target string) error {
	if item.StorageID != "local" || item.Bytes <= 0 || len(item.SHA256) != 64 {
		return errors.New("缺少有效的本地媒体大小或 SHA256，无法安全迁移")
	}
	if _, err := hex.DecodeString(item.SHA256); err != nil {
		return errors.New("SHA256 格式无效")
	}
	if !safeObjectKey(item.LocalPath) || !strings.HasPrefix(item.LocalPath, "media/") {
		return errors.New("本地媒体路径无效")
	}
	root, err := os.OpenRoot(cfg.DataDir)
	if err != nil {
		return errors.New("无法打开本地数据目录")
	}
	defer root.Close()
	f, err := root.Open(filepath.FromSlash(item.LocalPath))
	if err != nil {
		return errors.New("本地媒体不存在或不可读")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != item.Bytes {
		return errors.New("本地媒体类型或大小不符")
	}
	verify := func(r io.Reader) bool {
		h := sha256.New()
		n, e := io.Copy(h, io.LimitReader(r, item.Bytes+1))
		return e == nil && n == item.Bytes && hex.EncodeToString(h.Sum(nil)) == item.SHA256
	}
	if !verify(f) {
		return errors.New("本地文件 SHA256 校验失败，保留本地引用")
	}
	remote, err := NewRemote(cfg, target)
	if err != nil {
		return err
	}
	defer remote.Close()
	ext := strings.ToLower(path.Ext(item.LocalPath))
	rel := item.LocalPath
	if !strings.HasPrefix(rel, "media/users/") {
		rel, err = s.migrationMediaPath(ctx, item, ext)
		if err != nil {
			return err
		}
	}
	key := remote.Key(rel)
	// Reuse an identical object, but never overwrite a conflicting author path.
	valid := func() (bool, error) {
		r, err := remote.Open(ctx, key, 0)
		if err != nil {
			return false, err
		}
		defer r.Close()
		if !verify(r) {
			return false, errors.New("目标远端路径已存在但内容不同，未覆盖")
		}
		return true, nil
	}
	exists, err := valid()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !exists {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return errors.New("无法回读本地媒体")
		}
		contentType := mime.TypeByExtension(ext)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		if err = remote.Put(ctx, key, f, item.Bytes, contentType, item.SHA256); err != nil {
			return err
		}
		if ok, err := valid(); err != nil || !ok {
			return errors.New("远端文件回读或 SHA256 校验失败，保留本地引用")
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("无法开始媒体索引事务")
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE media SET storage_id=?,local_path='',object_key=?,updated_at=? WHERE post_id=? AND media_id=? AND status='done' AND storage_id='local' AND local_path=? AND bytes=? AND sha256=?`, target, key, now(), item.PostID, item.MediaID, item.LocalPath, item.Bytes, item.SHA256)
	if err != nil {
		return errors.New("远端副本已验证，但更新媒体索引失败；可安全重试")
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return errors.New("媒体索引已变化，未覆盖；请重试")
	}
	copyID := sha256.Sum256([]byte(item.PostID + "\x00" + item.MediaID + "\x00" + item.LocalPath))
	_, err = tx.ExecContext(ctx, database.Upsert(s.DB.Dialect,
		`INSERT INTO media_local_copies(copy_id,post_id,media_id,local_path,storage_id,object_key,bytes,sha256) VALUES(?,?,?,?,?,?,?,?)`,
		[]string{"copy_id"}, []string{"storage_id", "object_key", "bytes", "sha256"}),
		hex.EncodeToString(copyID[:]), item.PostID, item.MediaID, item.LocalPath, target, key, item.Bytes, item.SHA256)
	if err != nil {
		return errors.New("无法记录本地副本，媒体索引未切换")
	}
	return tx.Commit()
}
