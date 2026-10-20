package archive

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hirochachacha/go-smb2"
)

// SMB destinations use smb://host[:port]/share. Prefix is share-relative;
// DOMAIN\user is accepted without adding provider-specific database columns.
func (p StorageProfile) validateSMB() error {
	u, err := url.Parse(p.Endpoint)
	if err != nil || u.Scheme != "smb" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return errors.New("Samba 地址应为 smb://服务器/共享名，可指定端口；凭据请填入用户名和密码")
	}
	share := strings.TrimPrefix(u.Path, "/")
	if !safeSMBKey(share) || strings.Contains(share, "/") {
		return errors.New("Samba 地址只能包含一个共享名；子目录请填写到目录前缀")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("Samba 端口必须为 1–65535")
		}
	}
	if p.Prefix != "" && !safeSMBKey(p.Prefix) {
		return errors.New("Samba 目录前缀无效")
	}
	if strings.TrimSpace(p.Username) == "" || p.Password == "" {
		return errors.New("请填写 Samba 用户名和密码；域账号使用 域名\\用户名")
	}
	if parts := strings.Split(p.Username, `\`); len(parts) > 2 || parts[len(parts)-1] == "" || len(parts) == 2 && parts[0] == "" {
		return errors.New("Samba 用户名格式无效")
	}
	if p.UseProxy {
		return errors.New("Samba 使用直接 TCP 连接，不支持 HTTP/SOCKS 代理")
	}
	return nil
}

func safeSMBKey(key string) bool {
	if !safeObjectKey(key) || strings.ContainsAny(key, ":*?\"<>|") {
		return false
	}
	for _, part := range strings.Split(key, "/") {
		if strings.TrimRight(part, " .") != part {
			return false
		}
		for _, c := range part {
			if c < 32 {
				return false
			}
		}
	}
	return true
}

func smbError(err error) error {
	if err == nil {
		return nil
	}
	var response *smb2.ResponseError
	// Some servers use STATUS_NO_SUCH_FILE instead of OBJECT_NAME_NOT_FOUND.
	if errors.Is(err, os.ErrNotExist) || errors.As(err, &response) && response.Code == 0xc000000f {
		return fmt.Errorf("Samba 文件不存在: %w", os.ErrNotExist)
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	// SMB errors may contain server paths; never expose credentials or raw errors.
	return errors.New("Samba 操作失败，请检查网络、共享名、账号权限及超时设置")
}

// Each operation owns its connection, including streamed reads. Cancellation
// closes the socket as well as canceling SMB requests, bounding stalled servers.
func (b *Remote) smbShare(parent context.Context) (*smb2.Share, func(), error) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(b.profile.Timeout)*time.Second)
	u, _ := url.Parse(b.profile.Endpoint) // validated by NewRemote
	port := u.Port()
	if port == "" {
		port = "445"
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		cancel()
		return nil, nil, smbError(err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	var once sync.Once
	closeConn := func() { once.Do(func() { stop(); cancel(); _ = conn.Close() }) }
	user, domain := b.profile.Username, ""
	if parts := strings.SplitN(user, `\`, 2); len(parts) == 2 {
		domain, user = parts[0], parts[1]
	}
	d := smb2.Dialer{
		Negotiator: smb2.Negotiator{RequireMessageSigning: true},
		Initiator:  &smb2.NTLMInitiator{User: user, Password: b.profile.Password, Domain: domain},
	}
	session, err := d.DialContext(ctx, conn)
	if err != nil {
		closeConn()
		return nil, nil, smbError(err)
	}
	session = session.WithContext(ctx)
	share, err := session.Mount(strings.TrimPrefix(u.Path, "/"))
	if err != nil {
		closeConn()
		return nil, nil, smbError(err)
	}
	// Closing the transport releases every session/tree/handle, even on timeout.
	return share.WithContext(ctx), closeConn, nil
}

func (b *Remote) smbPut(ctx context.Context, key string, body io.Reader, size int64, expectedHash string) error {
	if !safeSMBKey(key) || size < 0 {
		return errors.New("Samba 对象路径或大小无效")
	}
	share, closeShare, err := b.smbShare(ctx)
	if err != nil {
		return err
	}
	defer closeShare()
	if err = share.MkdirAll(path.Dir(key), 0700); err != nil {
		return smbError(err)
	}
	tmp := key + ".xm-upload-" + rand.Text()
	f, err := share.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return smbError(err)
	}
	defer share.Remove(tmp)
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, size+1))
	hash := hex.EncodeToString(h.Sum(nil))
	if copyErr == nil && (n != size || expectedHash != "" && hash != expectedHash) {
		copyErr = errors.New("Samba 上传文件大小或 SHA256 不符")
	}
	if copyErr == nil {
		copyErr = f.Sync()
	}
	if closeErr := f.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return smbError(copyErr)
	}
	// Publish a complete file without overwriting an existing archive object.
	// A retry may reuse an identical object left before an interrupted DB commit.
	if err = share.Rename(tmp, key); err == nil {
		return nil
	}
	existing, openErr := share.Open(key)
	if openErr != nil {
		return smbError(err)
	}
	defer existing.Close()
	info, statErr := existing.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() != size {
		return errors.New("Samba 目标文件已存在但大小不符，未覆盖")
	}
	h.Reset()
	n, readErr := io.CopyN(h, existing, size)
	if readErr == nil && n == size && hex.EncodeToString(h.Sum(nil)) == hash {
		return nil
	}
	return errors.New("Samba 目标文件已存在但内容不符，未覆盖；请检查目标文件后重试")
}

func (b *Remote) smbSize(ctx context.Context, key string) (int64, error) {
	if !safeSMBKey(key) {
		return 0, errors.New("Samba 对象路径无效")
	}
	share, closeShare, err := b.smbShare(ctx)
	if err != nil {
		return 0, err
	}
	defer closeShare()
	info, err := share.Stat(key)
	if err != nil {
		return 0, smbError(err)
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("Samba 目标不是普通文件")
	}
	return info.Size(), nil
}

type smbReadCloser struct {
	file      *smb2.File
	close     func()
	remaining int64
}

func (r *smbReadCloser) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.file.Read(p)
	if n < 0 {
		n = 0
	}
	r.remaining -= int64(n)
	if err != nil && err != io.EOF {
		err = smbError(err)
	}
	return n, err
}
func (r *smbReadCloser) Close() error {
	err := r.file.Close()
	r.close()
	return smbError(err)
}

func (b *Remote) smbOpen(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	if !safeSMBKey(key) || offset < 0 {
		return nil, errors.New("Samba 路径或读取范围无效")
	}
	share, closeShare, err := b.smbShare(ctx)
	if err != nil {
		return nil, err
	}
	f, err := share.Open(key)
	var size int64
	if err == nil {
		var info os.FileInfo
		info, err = f.Stat()
		if err == nil {
			if !info.Mode().IsRegular() || offset > info.Size() {
				err = errors.New("invalid SMB file or offset")
			} else {
				size = info.Size() - offset
				_, err = f.Seek(offset, io.SeekStart)
			}
		}
	}
	if err != nil {
		closeShare()
		return nil, smbError(err)
	}
	return &smbReadCloser{file: f, close: closeShare, remaining: size}, nil
}

func (b *Remote) smbDelete(ctx context.Context, key string) error {
	if !safeSMBKey(key) {
		return errors.New("Samba 对象路径无效")
	}
	share, closeShare, err := b.smbShare(ctx)
	if err != nil {
		return err
	}
	defer closeShare()
	return smbError(share.Remove(key))
}
