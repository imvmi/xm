package archive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type storageFixture struct {
	mu          sync.Mutex
	objects     map[string][]byte
	fail        bool
	ignoreRange bool
}

func mockStorage(t *testing.T, kind string) (StorageProfile, *storageFixture) {
	t.Helper()
	f := &storageFixture{objects: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Cookie") != "" {
			t.Error("cookie leaked")
		}
		if kind == "webdav" {
			u, p, ok := r.BasicAuth()
			if !ok || u != "user" || p != "private-password" {
				w.WriteHeader(401)
				return
			}
		} else {
			if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=test-key/") || r.Header.Get("X-Amz-Security-Token") != "test-token" {
				t.Error("missing S3 signature or session token")
				w.WriteHeader(403)
				return
			}
		}
		if f.fail {
			w.WriteHeader(403)
			return
		}
		switch r.Method {
		case "MKCOL":
			w.WriteHeader(201)
		case "PUT":
			v, e := io.ReadAll(r.Body)
			if e != nil {
				t.Error(e)
			}
			f.objects[r.URL.Path] = v
			w.WriteHeader(200)
		case "DELETE":
			delete(f.objects, r.URL.Path)
			w.WriteHeader(204)
		case "HEAD", "GET":
			v, ok := f.objects[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			if r.Method == "HEAD" {
				w.Header().Set("Content-Length", strconv.Itoa(len(v)))
				return
			}
			if !f.ignoreRange && r.Header.Get("Range") != "" {
				var start int
				fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
				if start >= len(v) {
					w.WriteHeader(416)
					return
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(v)-1, len(v)))
				v = v[start:]
				w.Header().Set("Content-Length", strconv.Itoa(len(v)))
				w.WriteHeader(206)
			} else {
				w.Header().Set("Content-Length", strconv.Itoa(len(v)))
			}
			w.Write(v)
		default:
			w.WriteHeader(405)
		}
	}))
	t.Cleanup(srv.Close)
	return StorageProfile{ID: "test", Name: "Test", Kind: kind, Endpoint: srv.URL + "/root", Prefix: "xm", Username: "user", Password: "private-password", Bucket: "test-bucket", Region: "us-east-1", AccessKey: "test-key", SecretKey: "test-secret", SessionToken: "test-token", PathStyle: true, Timeout: 5}, f
}
func TestStorageProtocolsAndSeek(t *testing.T) {
	for _, kind := range []string{"webdav", "s3"} {
		t.Run(kind, func(t *testing.T) {
			p, f := mockStorage(t, kind)
			b, e := NewRemote(Config{StorageProfiles: []StorageProfile{p}}, p.ID)
			if e != nil {
				t.Fatal(e)
			}
			defer b.Close()
			ctx := context.Background()
			if e = b.Test(ctx); e != nil {
				t.Fatal(e)
			}
			f.mu.Lock()
			left := len(f.objects)
			f.mu.Unlock()
			if left != 0 {
				t.Fatal("probe not cleaned")
			}
			key := b.Key("media/ab/sample.mp4")
			payload := []byte("0123456789")
			if e = b.Put(ctx, key, bytes.NewReader(payload), 10, "video/mp4", "hash"); e != nil {
				t.Fatal(e)
			}
			if n, e := b.Size(ctx, key); e != nil || n != 10 {
				t.Fatal(n, e)
			}
			reader := &RemoteReader{Remote: b, Context: ctx, Key: key, Length: 10}
			defer reader.Close()
			reader.Seek(3, io.SeekStart)
			v, e := io.ReadAll(reader)
			if e != nil || string(v) != "3456789" {
				t.Fatal(string(v), e)
			}
			reader.Seek(-2, io.SeekEnd)
			v, e = io.ReadAll(reader)
			if e != nil || string(v) != "89" {
				t.Fatal(string(v), e)
			}
			if kind == "webdav" {
				f.mu.Lock()
				f.ignoreRange = true
				f.mu.Unlock()
				reader.Seek(4, io.SeekStart)
				v, e = io.ReadAll(reader)
				if e != nil || string(v) != "456789" {
					t.Fatal("ignored Range", string(v), e)
				}
			}
		})
	}
}
func TestRemoteUploadRetryAndStorageBinding(t *testing.T) {
	for _, kind := range []string{"webdav", "s3"} {
		t.Run(kind, func(t *testing.T) { testRemoteUploadRetryAndStorageBinding(t, kind) })
	}
}
func testRemoteUploadRetryAndStorageBinding(t *testing.T, kind string) {
	p, f := mockStorage(t, kind)
	dir := t.TempDir()
	cfg := Config{DataDir: dir, RequestTimeout: 5, MaxMediaBytes: 1024, MediaStorage: p.ID, StorageProfiles: []StorageProfile{p}}
	s, e := OpenStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = s.SavePosts("alice", []Post{{ID: "1", AuthorID: "123", Username: "alice", Assets: []Asset{{ID: "a", Kind: "video"}}}}); e != nil {
		t.Fatal(e)
	}
	PrivateDir(filepath.Join(dir, "media"))
	payload := []byte("cached movie")
	sum := sha256.Sum256(payload)
	hash := hex.EncodeToString(sum[:])
	j := MediaJob{PostID: "1", ID: "a", AuthorID: "123", Username: "alice"}
	j.Directory, e = s.MediaDirectory(j)
	if e != nil {
		t.Fatal(e)
	}
	rel, e := mediaRelativePath(j, ".mp4")
	if e != nil {
		t.Fatal(e)
	}
	if e = PrivateDir(filepath.Dir(filepath.Join(dir, rel))); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, rel), payload, 0600); e != nil {
		t.Fatal(e)
	}
	s.MediaResult(j, rel, int64(len(payload)), hash, fmt.Errorf("cached"))
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	result, e := DownloadPending(context.Background(), s, cfg, "alice", false)
	if e == nil || result.Failed != 1 {
		t.Fatal(result, e)
	}
	jobs, _ := s.MediaJobs("alice", true)
	if jobs[0].Path != rel || jobs[0].Status != "failed" || !validLocalMedia(dir, jobs[0]) {
		t.Fatal("staging lost", jobs)
	}
	f.mu.Lock()
	f.fail = false
	f.mu.Unlock()
	result, e = DownloadPending(context.Background(), s, cfg, "alice", false)
	if e != nil || result.Downloaded != 1 {
		t.Fatal(result, e)
	}
	jobs, _ = s.MediaJobs("alice", true)
	if jobs[0].StorageID != p.ID || jobs[0].ObjectKey != "xm/"+rel || jobs[0].Path != "" {
		t.Fatal("location lost", jobs)
	}
	if _, e = os.Stat(filepath.Join(dir, rel)); !os.IsNotExist(e) {
		t.Fatal("staging remains", e)
	}
	cfg.MediaStorage = "local"
	result, e = DownloadPending(context.Background(), s, cfg, "alice", true)
	if e != nil || result.Skipped != 1 {
		t.Fatal("switch moved old object", result, e)
	}
	f.mu.Lock()
	f.fail = true
	f.mu.Unlock()
	result, e = DownloadPending(context.Background(), s, cfg, "alice", true)
	if e == nil || result.Failed != 1 {
		t.Fatal(result, e)
	}
	jobs, _ = s.MediaJobs("alice", true)
	if jobs[0].Status != "done" || jobs[0].ObjectKey == "" {
		t.Fatal("outage erased saved object")
	}
}
func TestStorageValidationAndRedirects(t *testing.T) {
	p, _ := mockStorage(t, "webdav")
	for _, prefix := range []string{"../x", "/absolute", "x//y", "x/../z", "x\\z"} {
		q := p
		q.Prefix = prefix
		if q.Validate() == nil {
			t.Fatal("unsafe prefix", prefix)
		}
	}
	for _, endpoint := range []string{"file:///etc", "https://u:p@host/root", "https://host/root?token=secret", "https://host/#secret"} {
		q := p
		q.Endpoint = endpoint
		if q.Validate() == nil {
			t.Fatal("unsafe endpoint", endpoint)
		}
	}
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("credentials followed redirect") }))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer redirect.Close()
	p.Endpoint = redirect.URL
	b, e := NewRemote(Config{StorageProfiles: []StorageProfile{p}}, p.ID)
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	if e = b.Test(context.Background()); e == nil {
		t.Fatal("redirect accepted")
	}
}
