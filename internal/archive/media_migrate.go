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
	if storageErr != nil || (profile.Kind != "s3" && profile.Kind != "webdav") {
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
	id := sha256.Sum256([]byte(item.StorageID + "\x00" + item.Key + "\x00" + item.PostID + "\x00" + item.MediaID))
	rel := "media/migrated/" + hex.EncodeToString(id[:]) + "-" + item.SHA256 + ext
	root, err := os.OpenRoot(cfg.DataDir)
	if err != nil {
		return errors.New("无法打开本地数据目录")
	}
	defer root.Close()
	if err = root.MkdirAll("media/migrated", 0700); err != nil {
		return errors.New("无法创建本地媒体目录")
	}
	verify := func(f *os.File) bool {
		h := sha256.New()
		n, e := io.Copy(h, io.LimitReader(f, item.Bytes+1))
		return e == nil && n == item.Bytes && hex.EncodeToString(h.Sum(nil)) == item.SHA256
	}
	if f, e := root.Open(rel); e == nil {
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
		defer remote.client.CloseIdleConnections()
		r, e := remote.Open(ctx, item.Key, 0)
		if e != nil {
			return errors.New("远端读取失败，请检查网络、权限和源文件后重试")
		}
		defer r.Close()
		tmp := "media/migrated/.migration-" + rand.Text()
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
		if e = root.Rename(tmp, rel); e != nil {
			return errors.New("本地文件提交失败")
		}
	}
	// Also sync on a retry that found an already verified file: a previous
	// attempt may have stopped between rename, directory sync and DB commit.
	for _, dir := range []string{"media/migrated", "media", "."} {
		d, e := root.Open(dir)
		if e != nil {
			return errors.New("本地目录同步失败")
		}
		e = d.Sync()
		d.Close()
		if e != nil {
			return errors.New("本地目录同步失败")
		}
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

// RemoteizeMedia retains the local source and switches the index only after
// reading back and hashing the uploaded object. Object names are content based.
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
	id := sha256.Sum256([]byte(item.LocalPath + "\x00" + item.PostID + "\x00" + item.MediaID))
	ext := strings.ToLower(path.Ext(item.LocalPath))
	key := remote.Key("media/migrated/" + hex.EncodeToString(id[:]) + "-" + item.SHA256 + ext)
	// A verified object left by a failed index update need not be uploaded again.
	valid := func() bool {
		r, e := remote.Open(ctx, key, 0)
		if e != nil {
			return false
		}
		defer r.Close()
		return verify(r)
	}
	if !valid() {
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
		if !valid() {
			return errors.New("远端文件回读或 SHA256 校验失败，保留本地引用")
		}
	}
	result, err := s.DB.ExecContext(ctx, `UPDATE media SET storage_id=?,local_path='',object_key=?,updated_at=? WHERE post_id=? AND media_id=? AND status='done' AND storage_id='local' AND local_path=? AND bytes=? AND sha256=?`, target, key, now(), item.PostID, item.MediaID, item.LocalPath, item.Bytes, item.SHA256)
	if err != nil {
		return errors.New("远端副本已验证，但更新媒体索引失败；可安全重试")
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return errors.New("媒体索引已变化，未覆盖；请重试")
	}
	return nil
}
