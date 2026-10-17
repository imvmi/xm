package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type permanentMediaError struct{ message string }

func (e *permanentMediaError) Error() string { return e.message }

func permanentMedia(message string) error { return &permanentMediaError{message: message} }

func IsPermanentMediaError(err error) bool {
	var target *permanentMediaError
	return errors.As(err, &target)
}

func mediaURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" && u.Port() != "443" {
		return nil, errors.New("媒体 URL 必须使用受支持的 HTTPS 地址")
	}
	h := strings.ToLower(u.Hostname())
	if h != "pbs.twimg.com" && h != "video.twimg.com" {
		return nil, errors.New("媒体域名不在允许列表中（仅 pbs.twimg.com、video.twimg.com）")
	}
	return u, nil
}

// Download sends no account Cookie or Authorization to media hosts.
func Download(ctx context.Context, c *http.Client, dir string, j MediaJob, max int64) (string, int64, string, error) {
	if j.URL == "" {
		return "", 0, "", permanentMedia("该媒体未提供可下载的图片/MP4 地址；HLS 暂不支持")
	}
	u, err := mediaURL(j.URL)
	if err != nil {
		return "", 0, "", permanentMedia(err.Error())
	}
	client := *c
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("媒体重定向次数过多")
		}
		_, err := mediaURL(r.URL.String())
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", 0, "", errors.New("无法创建媒体请求")
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, "", NetworkError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", 0, "", fmt.Errorf("媒体下载返回 HTTP %d；可重新采集刷新链接后重试", resp.StatusCode)
	}
	if resp.ContentLength > max {
		return "", 0, "", permanentMedia("媒体超过配置的大小上限")
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	ext := ""
	if j.Kind == "photo" {
		switch ct {
		case "image/jpeg":
			ext = ".jpg"
		case "image/png":
			ext = ".png"
		case "image/webp":
			ext = ".webp"
		case "image/gif":
			ext = ".gif"
		case "image/avif":
			ext = ".avif"
		}
	} else if ct == "video/mp4" || ct == "application/mp4" {
		ext = ".mp4"
	}
	if ext == "" {
		return "", 0, "", permanentMedia("媒体响应类型不匹配，未保存可能的错误页")
	}
	rel, err := mediaRelativePath(j, ext)
	if err != nil {
		return "", 0, "", err
	}
	full := filepath.Join(dir, rel)
	if err = PrivateDir(filepath.Dir(full)); err != nil {
		return "", 0, "", errors.New("无法创建媒体目录")
	}
	f, err := os.CreateTemp(filepath.Dir(full), ".download-*")
	if err != nil {
		return "", 0, "", errors.New("无法创建媒体临时文件")
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, max+1))
	if err != nil {
		return "", 0, "", errors.New("媒体读取或写入失败，临时文件已移除")
	}
	if n > max {
		return "", 0, "", permanentMedia("媒体超过配置的大小上限")
	}
	if n == 0 {
		return "", 0, "", errors.New("媒体内容为空")
	}
	if err = f.Sync(); err != nil {
		return "", 0, "", errors.New("媒体同步到磁盘失败")
	}
	if err = f.Close(); err != nil {
		return "", 0, "", errors.New("关闭媒体文件失败")
	}
	if err = os.Rename(f.Name(), full); err != nil {
		return "", 0, "", errors.New("媒体文件提交失败")
	}
	return filepath.ToSlash(rel), n, hex.EncodeToString(h.Sum(nil)), nil
}

type DownloadSummary struct{ Downloaded, Skipped, Failed int }

func DownloadPending(ctx context.Context, s *Store, cfg Config, target string, verify bool) (DownloadSummary, error) {
	var summary DownloadSummary
	c, err := HTTPClient(cfg.Proxy, time.Duration(cfg.RequestTimeout)*time.Second)
	if err != nil {
		return summary, err
	}
	defer c.CloseIdleConnections()
	jobs, err := s.MediaJobs(target, verify)
	if err != nil {
		return summary, err
	}
	for _, j := range jobs {
		if err = ctx.Err(); err != nil {
			return summary, err
		}
		if j.Status == "done" {
			valid := false
			if j.StorageID == "local" || j.StorageID == "" {
				valid = validLocalMedia(cfg.DataDir, j)
			} else {
				valid, err = verifyRemote(ctx, cfg, j)
				// A temporary remote outage must not erase the saved location.
				if err != nil {
					summary.Failed++
					continue
				}
			}
			if valid {
				summary.Skipped++
				continue
			}
		} else {
			j.StorageID = cfg.ActiveStorage()
			j.ObjectKey = ""
		}
		j.Directory, err = s.MediaDirectory(j)
		if err != nil {
			return summary, err
		}
		localPath, size, hash := j.Path, j.Bytes, j.SHA256
		var downloadErr error
		if !validLocalMedia(cfg.DataDir, j) {
			localPath, size, hash, downloadErr = Download(ctx, c, cfg.DataDir, j, cfg.MaxMediaBytes)
		}
		savedPath := localPath
		if downloadErr == nil && j.StorageID != "local" && j.StorageID != "" {
			j.ObjectKey, downloadErr = uploadRemote(ctx, cfg, j.StorageID, localPath, size, hash)
			if downloadErr == nil {
				savedPath = ""
			}
		}
		if err = s.MediaResult(j, savedPath, size, hash, downloadErr); err != nil {
			return summary, err
		}
		if downloadErr == nil && savedPath == "" && localPath != "" {
			// Remove staging only after both upload verification and DB commit.
			_ = os.Remove(filepath.Join(cfg.DataDir, filepath.FromSlash(localPath)))
		}
		if downloadErr != nil {
			summary.Failed++
		} else {
			summary.Downloaded++
		}
	}
	if summary.Failed > 0 {
		return summary, fmt.Errorf("%d 个媒体下载失败，详情见数据库 media.last_error；使用 retry-media 重试", summary.Failed)
	}
	return summary, nil
}

func validLocalMedia(dir string, j MediaJob) bool {
	if j.Path == "" || filepath.IsAbs(j.Path) {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(j.Path))
	if !strings.HasPrefix(filepath.ToSlash(clean), "media/") || strings.HasPrefix(clean, "..") {
		return false
	}
	f, err := os.Open(filepath.Join(dir, clean))
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return err == nil && n == j.Bytes && hex.EncodeToString(h.Sum(nil)) == j.SHA256
}

func uploadRemote(ctx context.Context, cfg Config, id, localPath string, size int64, hash string) (string, error) {
	b, err := NewRemote(cfg, id)
	if err != nil {
		return "", err
	}
	defer b.Close()
	key := b.Key(localPath)
	f, err := os.Open(filepath.Join(cfg.DataDir, filepath.FromSlash(localPath)))
	if err != nil {
		return key, errors.New("无法打开媒体暂存文件")
	}
	defer f.Close()
	if err = b.Put(ctx, key, f, size, mime.TypeByExtension(filepath.Ext(localPath)), hash); err != nil {
		return key, err
	}
	n, err := b.Size(ctx, key)
	if err != nil {
		return key, err
	}
	if n != size {
		return key, errors.New("上传后远程文件长度不一致，暂存文件已保留")
	}
	return key, nil
}
func verifyRemote(ctx context.Context, cfg Config, j MediaJob) (bool, error) {
	b, err := NewRemote(cfg, j.StorageID)
	if err != nil {
		return false, err
	}
	defer b.Close()
	r, err := b.Open(ctx, j.ObjectKey, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer r.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, j.Bytes+1))
	if err != nil {
		return false, errors.New("远程媒体校验读取失败")
	}
	return n == j.Bytes && hex.EncodeToString(h.Sum(nil)) == j.SHA256, nil
}
