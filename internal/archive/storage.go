package archive

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Destinations are immutable: files retain the profile ID they were saved with.
// Credentials may be rotated without changing the destination.
type StorageProfile struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Endpoint     string `json:"endpoint"`
	Prefix       string `json:"prefix"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	Region       string `json:"region"`
	Bucket       string `json:"bucket"`
	AccessKey    string `json:"access_key"`
	SecretKey    string `json:"secret_key"`
	SessionToken string `json:"session_token"`
	PathStyle    bool   `json:"path_style"`
	UseProxy     bool   `json:"use_proxy"`
	Timeout      int    `json:"timeout_seconds"`
}

var storageID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func safeObjectKey(key string) bool {
	if key == "" || len(key) > 1024 || strings.ContainsAny(key, "\\\x00\r\n") || strings.HasPrefix(key, "/") {
		return false
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
func (p StorageProfile) Validate() error {
	if !storageID.MatchString(p.ID) || p.ID == "local" || strings.TrimSpace(p.Name) == "" || len(p.Name) > 100 {
		return errors.New("存储名称或标识无效")
	}
	if p.Kind != "webdav" && p.Kind != "s3" {
		return errors.New("存储类型必须为 WebDAV 或 S3")
	}
	if p.Timeout < 5 || p.Timeout > 3600 {
		return errors.New("存储请求超时必须为 5–3600 秒")
	}
	if p.Prefix != "" && !safeObjectKey(p.Prefix) {
		return errors.New("存储前缀必须为相对目录，不能包含空段或 ..")
	}
	if p.Endpoint != "" {
		u, err := url.Parse(p.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("存储地址必须为 HTTP(S) URL，不能包含凭据、查询参数或片段")
		}
	}
	if p.Kind == "webdav" && p.Endpoint == "" {
		return errors.New("请填写已存在的 WebDAV 根目录地址")
	}
	if p.Kind == "s3" && (!bucketName.MatchString(p.Bucket) || strings.Contains(p.Bucket, "..") || p.Region == "" || p.AccessKey == "" || p.SecretKey == "") {
		return errors.New("请填写有效的 S3 Bucket、Region、Access Key 和 Secret Key")
	}
	return nil
}
func (c Config) ActiveStorage() string {
	if c.MediaStorage == "" {
		return "local"
	}
	return c.MediaStorage
}
func (c Config) Storage(id string) (StorageProfile, error) {
	for _, p := range c.StorageProfiles {
		if p.ID == id {
			return p, nil
		}
	}
	return StorageProfile{}, errors.New("找不到媒体所属的存储连接，请恢复对应配置")
}
func (c Config) ValidateStorage() error {
	seen := map[string]bool{"local": true}
	for _, p := range c.StorageProfiles {
		if err := p.Validate(); err != nil {
			return err
		}
		if seen[p.ID] {
			return errors.New("存储标识重复")
		}
		seen[p.ID] = true
	}
	if !seen[c.ActiveStorage()] {
		return errors.New("当前存储连接不存在")
	}
	return nil
}
func (p StorageProfile) SameDestination(q StorageProfile) bool {
	return p.Kind == q.Kind && strings.TrimRight(p.Endpoint, "/") == strings.TrimRight(q.Endpoint, "/") && p.Prefix == q.Prefix && p.Region == q.Region && p.Bucket == q.Bucket && p.PathStyle == q.PathStyle
}

// Remote never sends the X account Cookie. Redirects are deliberately disabled
// so storage credentials cannot be forwarded to a different endpoint.
type Remote struct {
	profile StorageProfile
	client  *http.Client
	s3      *s3.Client
}

func NewRemote(cfg Config, id string) (*Remote, error) {
	p, err := cfg.Storage(id)
	if err != nil {
		return nil, err
	}
	if err = p.Validate(); err != nil {
		return nil, err
	}
	proxy := ""
	if p.UseProxy {
		proxy = cfg.Proxy
	}
	c, err := HTTPClient(proxy, time.Duration(p.Timeout)*time.Second)
	if err != nil {
		return nil, err
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	b := &Remote{profile: p, client: c}
	if p.Kind == "s3" {
		opts := s3.Options{Region: p.Region, Credentials: credentials.NewStaticCredentialsProvider(p.AccessKey, p.SecretKey, p.SessionToken), HTTPClient: c, UsePathStyle: p.PathStyle, RetryMaxAttempts: 2, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired}
		if p.Endpoint != "" {
			opts.BaseEndpoint = aws.String(p.Endpoint)
		}
		b.s3 = s3.New(opts)
	}
	return b, nil
}
func (b *Remote) Close()                     { b.client.CloseIdleConnections() }
func (b *Remote) Key(relative string) string { return path.Join(b.profile.Prefix, relative) }
func (b *Remote) request(ctx context.Context, method, key string, body io.Reader, size int64, headers map[string]string) (*http.Response, error) {
	if !safeObjectKey(key) {
		return nil, errors.New("无效的远程对象路径")
	}
	u, _ := url.Parse(b.profile.Endpoint)
	u.Path = strings.TrimRight(u.Path, "/") + "/" + key
	u.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, errors.New("无法创建存储请求")
	}
	if body != nil {
		req.ContentLength = size
	}
	if b.profile.Username != "" || b.profile.Password != "" {
		req.SetBasicAuth(b.profile.Username, b.profile.Password)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, errors.New("远程存储连接失败或超时，请检查地址、代理及证书")
	}
	return resp, nil
}
func storageError(err error) error {
	if err == nil {
		return nil
	}
	var e interface{ HTTPStatusCode() int }
	if errors.As(err, &e) {
		if e.HTTPStatusCode() == 404 {
			return fmt.Errorf("远程对象不存在: %w", os.ErrNotExist)
		}
		return fmt.Errorf("远程存储返回 HTTP %d", e.HTTPStatusCode())
	}
	return errors.New("远程存储操作失败，请检查连接、凭据和权限")
}
func (b *Remote) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType, hash string) error {
	if !safeObjectKey(key) {
		return errors.New("无效的远程对象路径")
	}
	if b.s3 != nil {
		if size > 5<<30 {
			return errors.New("S3 单文件上传上限为 5 GiB")
		}
		_, err := b.s3.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(b.profile.Bucket), Key: aws.String(key), Body: body, ContentLength: aws.Int64(size), ContentType: aws.String(contentType), Metadata: map[string]string{"sha256": hash}})
		return storageError(err)
	}
	parts := strings.Split(key, "/")
	for i := 1; i < len(parts); i++ {
		resp, err := b.request(ctx, "MKCOL", strings.Join(parts[:i], "/"), nil, 0, nil)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != 201 && resp.StatusCode != 405 {
			return fmt.Errorf("WebDAV 创建目录返回 HTTP %d", resp.StatusCode)
		}
	}
	resp, err := b.request(ctx, http.MethodPut, key, body, size, map[string]string{"Content-Type": contentType})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 && resp.StatusCode != 204 {
		return fmt.Errorf("WebDAV 上传返回 HTTP %d", resp.StatusCode)
	}
	return nil
}
func (b *Remote) Size(ctx context.Context, key string) (int64, error) {
	if !safeObjectKey(key) {
		return 0, errors.New("无效的远程对象路径")
	}
	if b.s3 != nil {
		v, err := b.s3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(b.profile.Bucket), Key: aws.String(key)})
		if err != nil {
			return 0, storageError(err)
		}
		return aws.ToInt64(v.ContentLength), nil
	}
	resp, err := b.request(ctx, http.MethodHead, key, nil, 0, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("WebDAV 读取属性返回 HTTP %d", resp.StatusCode)
	}
	return resp.ContentLength, nil
}
func (b *Remote) Open(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if !safeObjectKey(key) || offset < 0 {
		return nil, errors.New("无效的远程读取范围")
	}
	rangeValue := fmt.Sprintf("bytes=%d-", offset)
	var body io.ReadCloser
	var contentRange string
	if b.s3 != nil {
		v, err := b.s3.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(b.profile.Bucket), Key: aws.String(key), Range: aws.String(rangeValue)})
		if err != nil {
			return nil, storageError(err)
		}
		body = v.Body
		contentRange = aws.ToString(v.ContentRange)
	} else {
		resp, err := b.request(ctx, http.MethodGet, key, nil, 0, map[string]string{"Range": rangeValue})
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 && resp.StatusCode != 206 {
			resp.Body.Close()
			if resp.StatusCode == 404 {
				return nil, fmt.Errorf("远程对象不存在: %w", os.ErrNotExist)
			}
			return nil, fmt.Errorf("WebDAV 读取返回 HTTP %d", resp.StatusCode)
		}
		body = resp.Body
		if resp.StatusCode == 206 {
			contentRange = resp.Header.Get("Content-Range")
			if contentRange == "" {
				body.Close()
				return nil, errors.New("WebDAV 返回的范围响应无效")
			}
		}
	}
	if contentRange != "" {
		if !strings.HasPrefix(contentRange, fmt.Sprintf("bytes %d-", offset)) {
			body.Close()
			return nil, errors.New("远程存储返回了错误的读取范围")
		}
	} else if offset > 0 {
		if _, err := io.CopyN(io.Discard, body, offset); err != nil {
			body.Close()
			return nil, errors.New("远程媒体长度不足")
		}
	}
	return body, nil
}
func (b *Remote) Delete(ctx context.Context, key string) error {
	if !safeObjectKey(key) {
		return errors.New("无效的远程对象路径")
	}
	if b.s3 != nil {
		_, err := b.s3.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(b.profile.Bucket), Key: aws.String(key)})
		return storageError(err)
	}
	resp, err := b.request(ctx, http.MethodDelete, key, nil, 0, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		return fmt.Errorf("WebDAV 删除测试文件返回 HTTP %d", resp.StatusCode)
	}
	return nil
}
func (b *Remote) Test(ctx context.Context) error {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	payload := []byte("XM storage check " + hex.EncodeToString(nonce))
	key := b.Key(".xm-check/" + hex.EncodeToString(nonce) + ".txt")
	err := b.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), "text/plain", "")
	if err != nil {
		return err
	}
	checkErr := func() error {
		n, e := b.Size(ctx, key)
		if e != nil {
			return e
		}
		if n != int64(len(payload)) {
			return errors.New("远程测试文件长度不一致")
		}
		r, e := b.Open(ctx, key, 0)
		if e != nil {
			return e
		}
		defer r.Close()
		v, e := io.ReadAll(io.LimitReader(r, int64(len(payload))+1))
		if e != nil || !bytes.Equal(v, payload) {
			return errors.New("远程测试文件读取不一致")
		}
		return nil
	}()
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err = b.Delete(cleanupCtx, key); err != nil {
		return fmt.Errorf("测试文件清理失败（.xm-check 目录）：%w", err)
	}
	return checkErr
}

// RemoteReader gives ServeContent seek support without buffering whole videos.
// Services that ignore Range remain readable, at the cost of discarding bytes.
type RemoteReader struct {
	Remote  *Remote
	Context context.Context
	Key     string
	Length  int64
	pos     int64
	body    io.ReadCloser
}

func (r *RemoteReader) Prepare() error {
	if r.body != nil || r.pos >= r.Length {
		return nil
	}
	body, err := r.Remote.Open(r.Context, r.Key, r.pos)
	if err != nil {
		return err
	}
	r.body = body
	return nil
}
func (r *RemoteReader) Read(p []byte) (int, error) {
	if r.pos >= r.Length {
		return 0, io.EOF
	}
	if err := r.Prepare(); err != nil {
		return 0, err
	}
	if int64(len(p)) > r.Length-r.pos {
		p = p[:r.Length-r.pos]
	}
	n, err := r.body.Read(p)
	r.pos += int64(n)
	return n, err
}
func (r *RemoteReader) Seek(offset int64, whence int) (int64, error) {
	next := offset
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		next += r.pos
	case io.SeekEnd:
		next += r.Length
	default:
		return 0, errors.New("无效的 seek")
	}
	if next < 0 {
		return 0, errors.New("无效的偏移")
	}
	if next != r.pos {
		r.Close()
		r.pos = next
	}
	return next, nil
}
func (r *RemoteReader) Close() error {
	if r.body != nil {
		err := r.body.Close()
		r.body = nil
		return err
	}
	return nil
}
