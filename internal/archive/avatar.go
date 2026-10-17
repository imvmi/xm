package archive

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxAvatarBytes int64 = 5 << 20
const avatarRefreshInterval = 24 * time.Hour

type avatarMetadata struct {
	SourceURL    string    `json:"source_url"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	CheckedAt    time.Time `json:"checked_at"`
}

func AvatarFilename(userID string) (string, error) {
	if !userIDPattern.MatchString(userID) {
		return "", errors.New("头像用户 ID 无效")
	}
	return userID, nil
}

func AvatarExists(dataDir, userID string) bool {
	name, err := AvatarFilename(userID)
	if err != nil {
		return false
	}
	st, err := os.Stat(filepath.Join(dataDir, "avatars", name))
	return err == nil && st.Mode().IsRegular()
}

func avatarURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() != "pbs.twimg.com" || u.Port() != "" && u.Port() != "443" {
		return nil, errors.New("头像 URL 必须来自 pbs.twimg.com HTTPS 地址")
	}
	// X commonly returns a 48px _normal image. The documented image host also
	// serves a 400px variant at the same stable profile-image path.
	u.Path = strings.Replace(u.Path, "_normal.", "_400x400.", 1)
	return u, nil
}

// DownloadAvatar caches a public profile image locally. It never sends the X
// account Cookie or an Authorization header to the image host.
func DownloadAvatar(ctx context.Context, cfg Config, p Profile) error {
	client, err := HTTPClient(cfg.Proxy, time.Duration(cfg.RequestTimeout)*time.Second)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	return downloadAvatar(ctx, client, cfg.DataDir, p)
}

func downloadAvatar(ctx context.Context, base *http.Client, dataDir string, p Profile) error {
	if p.AvatarURL == "" {
		return nil
	}
	name, err := AvatarFilename(p.ID)
	if err != nil {
		return err
	}
	u, err := avatarURL(p.AvatarURL)
	if err != nil {
		return err
	}
	dir := filepath.Join(dataDir, "avatars")
	metaPath := filepath.Join(dir, name+".json")
	meta := readAvatarMetadata(metaPath)
	hasAvatar := AvatarExists(dataDir, p.ID)
	if hasAvatar && meta.SourceURL == p.AvatarURL && !meta.CheckedAt.IsZero() && time.Since(meta.CheckedAt) >= 0 && time.Since(meta.CheckedAt) < avatarRefreshInterval {
		return nil
	}
	client := *base
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("头像重定向次数过多")
		}
		_, err := avatarURL(r.URL.String())
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errors.New("无法创建头像请求")
	}
	if hasAvatar && meta.SourceURL == p.AvatarURL {
		if meta.ETag != "" {
			req.Header.Set("If-None-Match", meta.ETag)
		}
		if meta.LastModified != "" {
			req.Header.Set("If-Modified-Since", meta.LastModified)
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return NetworkError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified && hasAvatar && meta.SourceURL == p.AvatarURL {
		meta.CheckedAt = time.Now().UTC()
		return writeAvatarMetadata(metaPath, meta)
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New("头像下载失败")
	}
	if resp.ContentLength > maxAvatarBytes {
		return errors.New("头像文件过大")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAvatarBytes+1))
	if err != nil {
		return errors.New("无法读取头像")
	}
	if int64(len(data)) > maxAvatarBytes {
		return errors.New("头像文件过大")
	}
	if len(data) == 0 {
		return errors.New("头像内容为空")
	}
	switch http.DetectContentType(data) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		return errors.New("头像响应不是支持的图片格式")
	}
	if err = PrivateDir(dir); err != nil {
		return errors.New("无法创建头像目录")
	}
	if err = writePrivateFileAtomic(filepath.Join(dir, name), data); err != nil {
		return errors.New("无法保存头像")
	}
	meta = avatarMetadata{SourceURL: p.AvatarURL, ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"), CheckedAt: time.Now().UTC()}
	if err = writeAvatarMetadata(metaPath, meta); err != nil {
		return errors.New("头像已保存，但无法保存刷新信息")
	}
	return nil
}

func readAvatarMetadata(path string) avatarMetadata {
	var meta avatarMetadata
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &meta)
	}
	return meta
}

func writeAvatarMetadata(path string, meta avatarMetadata) error {
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err = PrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	return writePrivateFileAtomic(path, b)
}

func writePrivateFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".avatar-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
