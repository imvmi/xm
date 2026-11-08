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

type DownloadSummary struct {
	Downloaded, Skipped, Failed int
	ChangedPostIDs              []string `json:"-"`
}

// MediaDownloadError reports per-file failures; database/queue errors are separate.
type MediaDownloadError struct{ Count int }

func (e *MediaDownloadError) Error() string {
	return fmt.Sprintf("%d 个媒体下载失败，详情见数据库 media.last_error；使用 retry-media 重试", e.Count)
}

func DownloadPending(ctx context.Context, s *Store, cfg Config, target string, verify bool) (DownloadSummary, error) {
	var summary DownloadSummary
	c, err := HTTPClient(cfg.Proxy, time.Duration(cfg.RequestTimeout)*time.Second)
	if err != nil {
		return summary, err
	}
	defer c.CloseIdleConnections()
	return downloadPendingWithClient(ctx, s, cfg, target, verify, c)
}

func downloadPendingWithClient(ctx context.Context, s *Store, cfg Config, target string, verify bool, c *http.Client) (DownloadSummary, error) {
	s.mediaStagingMu.Lock()
	defer s.mediaStagingMu.Unlock()
	var summary DownloadSummary
	jobs, err := s.MediaJobs(target, verify)
	if err != nil {
		return summary, err
	}
	var budget *mediaStagingBudget
	defer func() {
		if budget != nil {
			budget.root.Close()
		}
	}()
	for _, j := range jobs {
		if err = ctx.Err(); err != nil {
			return summary, err
		}
		current, checkErr := s.MediaJobCurrent(j)
		if checkErr != nil {
			return summary, checkErr
		}
		if !current {
			continue
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
		j.RetryInitialSeconds, j.RetryMaxSeconds = cfg.MediaRetryInitial, cfg.MediaRetryMax
		var downloadErr error
		if !validLocalMedia(cfg.DataDir, j) {
			if budget == nil {
				budget, err = s.newMediaStagingBudget(ctx, cfg)
				if err != nil {
					return summary, err
				}
			}
			capacity := budget.capacity(j)
			var stageErr error
			localPath, size, hash, downloadErr = downloadMediaWithCapacity(ctx, c, cfg.DataDir, j, cfg.MaxMediaBytes, capacity, func(path string, size int64, hash string) error {
				stageErr = s.StageMedia(&j, path, size, hash)
				return stageErr
			})
			if errors.Is(stageErr, ErrMediaChanged) {
				budget.refresh(j, localPath, false)
				continue
			}
			if stageErr != nil {
				return summary, stageErr
			}
			if downloadErr != nil && j.Path != "" {
				// Publication may have failed after staging was committed. Preserve its
				// reference and complete checkpoint for a retry without another GET.
				localPath, size, hash = j.Path, j.Bytes, j.SHA256
			}
		}
		savedPath := localPath
		if downloadErr == nil && j.StorageID != "local" && j.StorageID != "" {
			j.ObjectKey, downloadErr = uploadRemote(ctx, cfg, j.StorageID, localPath, size, hash)
			if downloadErr == nil {
				savedPath = ""
			}
		}
		// A stopped task leaves its durable queue entry untouched, without
		// consuming a retry or imposing a new backoff for user cancellation.
		if err = ctx.Err(); err != nil {
			return summary, err
		}
		if err = s.MediaResult(j, savedPath, size, hash, downloadErr); errors.Is(err, ErrMediaChanged) {
			// URL refresh can invalidate a staged result; ignoring media may retain it.
			// Remove only a file that no current media row references.
			if localPath != "" {
				var refs int
				if e := s.DB.QueryRow("SELECT COUNT(*) FROM media WHERE local_path=?", localPath).Scan(&refs); e != nil {
					return summary, e
				} else if refs == 0 {
					_ = os.Remove(filepath.Join(cfg.DataDir, filepath.FromSlash(localPath)))
				}
			}
			if budget != nil {
				budget.refresh(j, localPath, false)
			}
			continue
		} else if err != nil {
			return summary, err
		}
		summary.ChangedPostIDs = append(summary.ChangedPostIDs, j.PostID)
		if downloadErr == nil && savedPath == "" && localPath != "" {
			// Remove staging only after both upload verification and DB commit.
			_ = os.Remove(filepath.Join(cfg.DataDir, filepath.FromSlash(localPath)))
		}
		if budget != nil {
			budget.refresh(j, localPath, downloadErr == nil)
		}
		if downloadErr != nil {
			summary.Failed++
		} else {
			summary.Downloaded++
		}
	}
	if summary.Failed > 0 {
		return summary, &MediaDownloadError{Count: summary.Failed}
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
	// HEAD alone cannot detect same-length corruption. Keep the local original
	// until the uploaded representation has been read back and verified.
	r, err := b.Open(ctx, key, 0)
	if err != nil {
		return key, errors.New("上传后无法回读远程文件，暂存文件已保留")
	}
	h := sha256.New()
	n, readErr := io.Copy(h, io.LimitReader(r, size+1))
	closeErr := r.Close()
	if readErr != nil || closeErr != nil || n != size || hex.EncodeToString(h.Sum(nil)) != hash {
		return key, errors.New("上传后远程文件 SHA256 校验失败，暂存文件已保留")
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
