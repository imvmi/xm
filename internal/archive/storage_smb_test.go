package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xm/internal/testutil"
)

func smbProfile(endpoint string) StorageProfile {
	return StorageProfile{ID: "samba", Name: "Samba", Kind: "smb", Endpoint: endpoint, Prefix: "测试归档", Username: "fixture-user", Password: "fixture-password", Timeout: 5}
}

func TestSMBValidation(t *testing.T) {
	for _, endpoint := range []string{"smb://nas/share", "smb://127.0.0.1:1445/share", "smb://[::1]/共享"} {
		if err := smbProfile(endpoint).Validate(); err != nil {
			t.Fatal(endpoint, err)
		}
	}
	for _, endpoint := range []string{"https://nas/share", "smb://u:p@nas/share", "smb://nas", "smb://nas/share/subdir", "smb://nas/..", "smb://nas/share?password=x", "smb://nas/share#x", "smb://nas:0/share", "smb://nas:65536/share", "smb://nas/share%5C..", "smb://nas/share:ads"} {
		if err := smbProfile(endpoint).Validate(); err == nil {
			t.Error("accepted invalid SMB endpoint", endpoint)
		}
	}
	for _, key := range []string{"../x", "/abs", `a\b`, "a//b", "a/../b", "x:stream", "a. /b", "a/*", "a/\x01"} {
		if safeSMBKey(key) {
			t.Error("accepted unsafe SMB key", key)
		}
	}
	p := smbProfile("smb://nas/share")
	p.UseProxy = true
	if p.Validate() == nil {
		t.Fatal("SMB silently accepted an HTTP proxy")
	}
}

func TestSMBWireStorage(t *testing.T) {
	endpoint, dir := testutil.SMBFixture(t)
	p := smbProfile(endpoint)
	cfg := DefaultConfig()
	cfg.StorageProfiles = []StorageProfile{p}
	r, err := NewRemote(cfg, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	if err = r.Test(ctx); err != nil {
		t.Fatal("read/write/delete connection test", err)
	}
	payload := strings.Repeat("视频数据", 50000)
	h := sha256.Sum256([]byte(payload))
	key := r.Key("media/片段.mp4")
	put := func() error {
		return r.Put(ctx, key, strings.NewReader(payload), int64(len(payload)), "video/mp4", hex.EncodeToString(h[:]))
	}
	if err = put(); err != nil {
		t.Fatal("put", err)
	}
	if err = put(); err != nil {
		t.Fatal("identical retry", err)
	}
	size, err := r.Size(ctx, key)
	if err != nil || size != int64(len(payload)) {
		t.Fatal("size", size, err)
	}
	for _, offset := range []int64{0, 12345, int64(len(payload)) - 5} {
		body, err := r.Open(ctx, key, offset)
		if err != nil {
			t.Fatal("open", err)
		}
		got, readErr := io.ReadAll(body)
		closeErr := body.Close()
		if readErr != nil || closeErr != nil || string(got) != payload[offset:] {
			t.Fatal("stream/range", offset, len(got), readErr, closeErr)
		}
	}
	if err = r.Put(ctx, key, strings.NewReader("changed"), 7, "", ""); err == nil {
		t.Fatal("overwrote an existing archive object")
	}
	if err = r.Put(ctx, r.Key("media/short.jpg"), strings.NewReader("short"), 100, "", ""); err == nil {
		t.Fatal("accepted truncated upload")
	}
	if _, err = os.Stat(filepath.Join(dir, filepath.FromSlash(r.Key("media/short.jpg")))); !os.IsNotExist(err) {
		t.Fatal("published truncated upload", err)
	}
	if err = r.Delete(ctx, key); err != nil {
		t.Fatal("delete", err)
	}
	if _, err = r.Open(ctx, key, 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing file mapping", err)
	}
	p.Password = "wrong-secret"
	cfg.StorageProfiles = []StorageProfile{p}
	bad, _ := NewRemote(cfg, p.ID)
	defer bad.Close()
	if err = bad.Test(ctx); err == nil || strings.Contains(err.Error(), p.Password) {
		t.Fatal("bad credentials accepted or exposed", err)
	}
}

func TestSMBCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			_, _ = io.Copy(io.Discard, conn)
		}
	}()
	p := smbProfile("smb://" + listener.Addr().String() + "/archive")
	cfg := DefaultConfig()
	cfg.StorageProfiles = []StorageProfile{p}
	r, _ := NewRemote(cfg, p.ID)
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err = r.Open(ctx, "media/a.jpg", 0); err == nil || time.Since(start) > time.Second {
		t.Fatal("stalled SMB server did not cancel", err)
	}
}
