package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const mediaChunkBytes int64 = 8 << 20

// Only a strong ETag permits combining responses across requests. URLs are
// fingerprinted so refreshing a signed URL never reuses an unrelated prefix.
type mediaCheckpoint struct {
	ResolvedURLHash string `json:"resolved_url_hash"`
	URLHash         string `json:"url_hash"`
	Kind            string `json:"kind"`
	ETag            string `json:"etag"`
	Ext             string `json:"ext"`
	Total           int64  `json:"total"`
	Bytes           int64  `json:"bytes"`
	SHA256          string `json:"sha256"`
}

func mediaExtension(kind, contentType string) string {
	ct, _, _ := mime.ParseMediaType(contentType)
	if kind == "photo" {
		return map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif", "image/avif": ".avif"}[ct]
	}
	if ct == "video/mp4" || ct == "application/mp4" {
		return ".mp4"
	}
	return ""
}

func strongMediaETag(s string) bool {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return false
	}
	for _, c := range []byte(s[1 : len(s)-1]) {
		if c < 0x21 || c == 0x22 || c == 0x7f {
			return false
		}
	}
	return true
}

func mediaContentRange(s string) (start, end, total int64, err error) {
	invalid := errors.New("媒体续传响应的字节范围无效")
	if !strings.HasPrefix(s, "bytes ") {
		return 0, 0, 0, invalid
	}
	parts := strings.Split(strings.TrimPrefix(s, "bytes "), "/")
	if len(parts) != 2 {
		return 0, 0, 0, invalid
	}
	bounds := strings.Split(parts[0], "-")
	if len(bounds) != 2 {
		return 0, 0, 0, invalid
	}
	start, e1 := strconv.ParseInt(bounds[0], 10, 64)
	end, e2 := strconv.ParseInt(bounds[1], 10, 64)
	total, e3 := strconv.ParseInt(parts[1], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || start < 0 || end < start || total <= end {
		return 0, 0, 0, invalid
	}
	return start, end, total, nil
}

func saveMediaCheckpoint(path string, m mediaCheckpoint) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".resume-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncMediaDirectory(filepath.Dir(path))
}

func syncMediaDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return SyncDirectory(d)
}

// The metadata commits only fsynced bytes. After a crash, discard any suffix
// written after the last checkpoint, and verify the prefix before sending Range.
func loadMediaCheckpoint(part, meta, urlHash, kind string, max int64) (mediaCheckpoint, hash.Hash) {
	var m mediaCheckpoint
	h := sha256.New()
	data, err := os.ReadFile(meta)
	if err == nil && len(data) <= 16384 {
		err = json.Unmarshal(data, &m)
	} else {
		err = errors.New("missing checkpoint")
	}
	if err == nil && m.URLHash == urlHash && m.ResolvedURLHash != "" && m.Kind == kind && (strongMediaETag(m.ETag) || m.Bytes == m.Total) && m.Bytes > 0 && m.Bytes <= m.Total && m.Total <= max && m.Ext != "" {
		f, e := os.OpenFile(part, os.O_RDWR, 0600)
		if e == nil {
			defer f.Close()
			info, e := f.Stat()
			if e == nil && info.Mode().IsRegular() && info.Size() >= m.Bytes {
				n, e := io.Copy(h, io.LimitReader(f, m.Bytes))
				if e == nil && n == m.Bytes && hex.EncodeToString(h.Sum(nil)) == m.SHA256 && f.Truncate(m.Bytes) == nil {
					return m, h
				}
			}
		}
	}
	_ = os.Remove(part)
	_ = os.Remove(meta)
	return mediaCheckpoint{}, sha256.New()
}

type mediaFileWriter struct {
	f   *os.File
	err error
}

func (w *mediaFileWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	if err != nil {
		w.err = err
	}
	return n, err
}

// Download sends no account credentials to media hosts. One request at a time,
// bounded ranges, and If-Range protect both bandwidth and representation identity.
func Download(ctx context.Context, c *http.Client, dir string, j MediaJob, max int64) (string, int64, string, error) {
	return downloadMedia(ctx, c, dir, j, max, nil)
}

// beforePublish records the verified staging reference before the file rename.
// A crash on either side is recoverable from the complete checkpoint or DB path.
func downloadMedia(ctx context.Context, c *http.Client, dir string, j MediaJob, max int64, beforePublish func(string, int64, string) error) (string, int64, string, error) {
	return downloadMediaWithCapacity(ctx, c, dir, j, max, max, beforePublish)
}

func downloadMediaWithCapacity(ctx context.Context, c *http.Client, dir string, j MediaJob, max, capacity int64, beforePublish func(string, int64, string) error) (string, int64, string, error) {
	if capacity <= 0 {
		return "", 0, "", errMediaStagingFull
	}

	if err := ctx.Err(); err != nil {
		return "", 0, "", err
	}
	if j.URL == "" {
		return "", 0, "", permanentMedia("该媒体未提供可下载的图片/MP4 地址；HLS 暂不支持")
	}
	u, err := mediaURL(j.URL)
	if err != nil {
		return "", 0, "", permanentMedia(err.Error())
	}
	if max <= 0 {
		return "", 0, "", permanentMedia("媒体大小上限无效")
	}
	// A deterministic, media-specific staging path survives service restarts, but
	// is never exposed as an archived file or uploaded until fully verified.
	base, err := mediaRelativePath(j, ".mp4")
	if err != nil {
		return "", 0, "", err
	}
	parent := filepath.Join(dir, filepath.Dir(base))
	if err = PrivateDir(parent); err != nil {
		return "", 0, "", errors.New("无法创建媒体目录")
	}
	stem := strings.TrimSuffix(filepath.Base(base), ".mp4")
	part := filepath.Join(parent, ".download-"+stem+".part")
	meta := part + ".json"
	urlSum := sha256.Sum256([]byte(j.URL))
	urlHash := hex.EncodeToString(urlSum[:])
	m, h := loadMediaCheckpoint(part, meta, urlHash, j.Kind, max)
	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", 0, "", errors.New("无法创建媒体断点文件")
	}
	keep := m.Bytes > 0
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(part)
			_ = os.Remove(meta)
		}
	}()
	client := *c
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("媒体重定向次数过多")
		}
		_, err := mediaURL(r.URL.String())
		return err
	}
	if m.Total > capacity {
		return "", 0, "", errMediaStagingFull
	}
	offset := m.Bytes
	// A single fallback avoids endless loops with servers that send invalid ranges.
	ranged, fallback := true, false
	reset := func() error {
		keep = false
		m = mediaCheckpoint{}
		offset = 0
		h.Reset()
		_ = os.Remove(meta)
		if err := f.Truncate(0); err != nil {
			return err
		}
		_, err := f.Seek(0, io.SeekStart)
		return err
	}
	checkpoint := func() error {
		if !strongMediaETag(m.ETag) || m.Total <= 0 || offset <= 0 {
			return nil
		}
		if err := f.Sync(); err != nil {
			return err
		}
		m.Bytes = offset
		m.SHA256 = hex.EncodeToString(h.Sum(nil))
		if err := saveMediaCheckpoint(meta, m); err != nil {
			return err
		}
		keep = true
		return nil
	}
	for m.Total == 0 || offset < m.Total {
		if err = ctx.Err(); err != nil {
			return "", 0, "", err
		}
		end := offset + min(mediaChunkBytes, max-offset) - 1
		if m.Total > 0 {
			end = min(end, m.Total-1)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return "", 0, "", errors.New("无法创建媒体请求")
		}
		req.Header.Set("Accept-Encoding", "identity")
		if ranged {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))
			if offset > 0 {
				req.Header.Set("If-Range", m.ETag)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", 0, "", mediaResumeError(NetworkError(err), offset, keep)
		}
		resolvedURL := u.String()
		if resp.Request != nil && resp.Request.URL != nil {
			resolvedURL = resp.Request.URL.String()
		}
		resolvedSum := sha256.Sum256([]byte(resolvedURL))
		resolvedHash := hex.EncodeToString(resolvedSum[:])
		ext := mediaExtension(j.Kind, resp.Header.Get("Content-Type"))
		etag := resp.Header.Get("ETag")
		total, expected := resp.ContentLength, resp.ContentLength
		invalidRange := false
		if resp.StatusCode == http.StatusPartialContent {
			start, last, length, rangeErr := mediaContentRange(resp.Header.Get("Content-Range"))
			invalidRange = rangeErr != nil || !ranged || start != offset || last > end || (resp.ContentLength > 0 && resp.ContentLength != last-start+1)
			total, expected = length, last-start+1
			if offset > 0 {
				invalidRange = invalidRange || m.ETag != etag || m.Total != total || m.Ext != ext || m.ResolvedURLHash != resolvedHash
			}
			// A partial representation without a strong validator cannot be combined.
			if !strongMediaETag(etag) && last+1 < total {
				invalidRange = true
			}
		} else if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			invalidRange = true
		} else if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return "", 0, "", mediaResumeError(fmt.Errorf("媒体下载返回 HTTP %d；可重新采集刷新链接后重试", resp.StatusCode), offset, keep)
		}
		if invalidRange {
			resp.Body.Close()
			if err = reset(); err != nil {
				return "", 0, "", errors.New("无法重置媒体断点")
			}
			if fallback {
				return "", 0, "", errors.New("媒体服务器返回无效的续传响应，请稍后重试")
			}
			ranged = false
			fallback = true
			continue
		}
		if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
			resp.Body.Close()
			keep = false
			return "", 0, "", errors.New("媒体服务器未返回原始字节，无法安全续传")
		}
		if total > max {
			resp.Body.Close()
			keep = false
			return "", 0, "", permanentMedia("媒体超过配置的大小上限")
		}
		if total > capacity {
			resp.Body.Close()
			return "", 0, "", errMediaStagingFull
		}
		if ext == "" {
			resp.Body.Close()
			keep = false
			return "", 0, "", permanentMedia("媒体响应类型不匹配，未保存可能的错误页")
		}
		if resp.StatusCode == http.StatusOK {
			// Range ignored or If-Range failed: this is a new full body, never append it.
			if err = reset(); err != nil {
				resp.Body.Close()
				return "", 0, "", errors.New("无法重置媒体断点")
			}
		}
		m = mediaCheckpoint{URLHash: urlHash, ResolvedURLHash: resolvedHash, Kind: j.Kind, ETag: etag, Ext: ext, Total: maxInt64Zero(total), Bytes: offset}
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			resp.Body.Close()
			return "", 0, "", errors.New("无法定位媒体断点")
		}
		before := offset
		limit := min(max, capacity) - offset
		if expected > 0 {
			limit = min(limit, expected)
		}
		writer := &mediaFileWriter{f: f}
		n, readErr := io.Copy(io.MultiWriter(writer, h), io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		if writer.err != nil {
			// The previous checkpoint still describes a valid prefix. Do not commit a
			// short disk write whose hash may not have been advanced by MultiWriter.
			_ = f.Truncate(before)
			return "", 0, "", errors.New("媒体写入磁盘失败，请检查磁盘空间和权限")
		}
		offset += n
		if n > limit {
			keep = false
			if offset > max {
				return "", 0, "", permanentMedia("媒体超过配置的大小上限")
			}
			if offset > capacity {
				return "", 0, "", errMediaStagingFull
			}
			return "", 0, "", errors.New("媒体内容超过声明的响应长度，已丢弃不可靠断点")
		}
		if readErr == nil && expected > 0 && n != expected {
			readErr = io.ErrUnexpectedEOF
		}
		if err = checkpoint(); err != nil {
			return "", 0, "", errors.New("媒体断点保存失败，请检查磁盘空间和权限")
		}
		if readErr != nil {
			reason := errors.New("媒体传输中断或请求超时")
			if ctx.Err() != nil {
				reason = ctx.Err()
			}
			return "", 0, "", mediaResumeError(reason, offset, keep)
		}
		if offset == 0 {
			return "", 0, "", errors.New("媒体内容为空")
		}
		if resp.StatusCode == http.StatusOK || offset == m.Total {
			break
		}
	}
	if err = ctx.Err(); err != nil {
		return "", 0, "", err
	}
	rel, err := mediaRelativePath(j, m.Ext)
	if err != nil {
		keep = false
		return "", 0, "", err
	}
	if err = f.Sync(); err != nil {
		return "", 0, "", errors.New("媒体同步到磁盘失败")
	}
	if beforePublish != nil {
		// A fully received response can be recovered locally even without an ETag.
		m.Total, m.Bytes, m.SHA256 = offset, offset, hex.EncodeToString(h.Sum(nil))
		if err = saveMediaCheckpoint(meta, m); err != nil {
			return "", 0, "", errors.New("媒体完成断点保存失败")
		}
		keep = true
		if err = beforePublish(filepath.ToSlash(rel), offset, m.SHA256); err != nil {
			if errors.Is(err, ErrMediaChanged) {
				keep = false
			}
			return "", 0, "", err
		}
	}
	if err = f.Close(); err != nil {
		return "", 0, "", errors.New("关闭媒体文件失败")
	}
	if err = os.Rename(part, filepath.Join(dir, rel)); err != nil {
		return "", 0, "", errors.New("媒体文件提交失败")
	}
	if err = syncMediaDirectory(parent); err != nil {
		return "", 0, "", errors.New("媒体目录同步失败")
	}
	keep = false
	return filepath.ToSlash(rel), offset, hex.EncodeToString(h.Sum(nil)), nil
}

func maxInt64Zero(n int64) int64 {
	if n > 0 {
		return n
	}
	return 0
}
func mediaResumeError(err error, offset int64, retained bool) error {
	if retained && offset > 0 {
		return fmt.Errorf("%w；已保留 %d 字节，后续重试将尝试断点续传", err, offset)
	}
	return err
}

var errMediaStagingFull = errors.New("本地下载暂存容量不足，请在设置 → 爬虫 → 媒体下载与重试清理断点，或先完成待上传文件")
