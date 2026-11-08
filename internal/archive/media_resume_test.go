package archive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type brokenMediaReader struct{ err error }

func (r brokenMediaReader) Read([]byte) (int, error) { return 0, r.err }

func resumeMediaFixture() MediaJob {
	return MediaJob{AuthorID: "42", Username: "alice", PostID: "100", ID: "video", Kind: "video", URL: "https://video.twimg.com/fixture.mp4"}
}
func mediaResponse(status int, contentRange, etag string, body io.Reader, length int64) *http.Response {
	h := http.Header{"Content-Type": []string{"video/mp4"}}
	if contentRange != "" {
		h.Set("Content-Range", contentRange)
	}
	if etag != "" {
		h.Set("ETag", etag)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(body), ContentLength: length}
}
func interruptedPrefix(t *testing.T, dir string, j MediaJob) {
	t.Helper()
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Range") != "bytes=0-5" || r.Header.Get("Accept-Encoding") != "identity" {
			t.Error("incorrect initial range", r.Header)
		}
		return mediaResponse(206, "bytes 0-5/6", `"v1"`, io.MultiReader(strings.NewReader("abc"), brokenMediaReader{io.ErrUnexpectedEOF}), 6), nil
	})}
	path, _, _, err := Download(context.Background(), c, dir, j, 6)
	if err == nil || path != "" || !strings.Contains(err.Error(), "已保留 3 字节") {
		t.Fatal(path, err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "media", "users", "*", "*.mp4"))
	if len(files) != 0 {
		t.Fatal("partial file published", files)
	}
}
func assertMediaPayload(t *testing.T, dir, path string, size int64, hash string, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil || string(data) != want || size != int64(len(want)) {
		t.Fatal(path, size, string(data), err)
	}
	if !validLocalMedia(dir, MediaJob{Path: path, Bytes: size, SHA256: hash}) {
		t.Fatal("incorrect final hash")
	}
	parts, _ := filepath.Glob(filepath.Join(dir, "media", "users", "*", ".download-*"))
	if len(parts) != 0 {
		t.Fatal("checkpoint not cleaned after success", parts)
	}
}

func TestMediaResumeAfterInterruptionAndRestart(t *testing.T) {
	for _, suffix := range []string{"", "uncommitted crash suffix"} {
		t.Run(fmt.Sprintf("suffix_%t", suffix != ""), func(t *testing.T) {
			dir, j := t.TempDir(), resumeMediaFixture()
			interruptedPrefix(t, dir, j)
			if suffix != "" {
				parts, _ := filepath.Glob(filepath.Join(dir, "media", "users", "*", "*.part"))
				f, err := os.OpenFile(parts[0], os.O_WRONLY|os.O_APPEND, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString(suffix)
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			requests := 0
			c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.Header.Get("Range") != "bytes=3-5" || r.Header.Get("If-Range") != `"v1"` {
					t.Error("prefix was not resumed", r.Header)
				}
				if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
					t.Error("credentials leaked")
				}
				return mediaResponse(206, "bytes 3-5/6", `"v1"`, strings.NewReader("def"), 3), nil
			})}
			path, size, hash, err := Download(context.Background(), c, dir, j, 6)
			if err != nil || requests != 1 {
				t.Fatal(requests, err)
			}
			assertMediaPayload(t, dir, path, size, hash, "abcdef")
		})
	}
}

func TestMediaResumeSafeFallback(t *testing.T) {
	for _, scenario := range []string{"ignored_range", "etag_changed", "wrong_offset", "wrong_total", "wrong_length", "malformed_range", "redirect_changed", "416"} {
		t.Run(scenario, func(t *testing.T) {
			dir, j := t.TempDir(), resumeMediaFixture()
			interruptedPrefix(t, dir, j)
			requests := 0
			c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				if requests > 2 {
					t.Fatal("fallback loop")
				}
				if requests == 2 {
					if r.Header.Get("Range") != "" || r.Header.Get("If-Range") != "" {
						t.Error("fallback did not request a fresh body")
					}
					return mediaResponse(200, "", `"v2"`, strings.NewReader("uvwxyz"), 6), nil
				}
				switch scenario {
				case "ignored_range":
					return mediaResponse(200, "", `"v2"`, strings.NewReader("uvwxyz"), 6), nil
				case "etag_changed":
					return mediaResponse(206, "bytes 3-5/6", `"v2"`, strings.NewReader("xyz"), 3), nil
				case "wrong_offset":
					return mediaResponse(206, "bytes 2-4/6", `"v1"`, strings.NewReader("xyz"), 3), nil
				case "wrong_total":
					return mediaResponse(206, "bytes 3-5/7", `"v1"`, strings.NewReader("xyz"), 3), nil
				case "wrong_length":
					return mediaResponse(206, "bytes 3-5/6", `"v1"`, strings.NewReader("xyz"), 4), nil
				case "redirect_changed":
					response := mediaResponse(206, "bytes 3-5/6", `"v1"`, strings.NewReader("xyz"), 3)
					response.Request = &http.Request{URL: &url.URL{Scheme: "https", Host: "video.twimg.com", Path: "/different.mp4"}}
					return response, nil
				case "malformed_range":
					return mediaResponse(206, "bytes */6", `"v1"`, strings.NewReader("xyz"), 3), nil
				default:
					return mediaResponse(416, "bytes */2", `"v1"`, strings.NewReader(""), 0), nil
				}
			})}
			path, size, hash, err := Download(context.Background(), c, dir, j, 6)
			if err != nil {
				t.Fatal(err)
			}
			assertMediaPayload(t, dir, path, size, hash, "uvwxyz")
		})
	}
}

func TestMediaResumeDiscardsChangedURLAndCorruptPrefix(t *testing.T) {
	for _, scenario := range []string{"url", "corrupt", "metadata"} {
		t.Run(scenario, func(t *testing.T) {
			dir, j := t.TempDir(), resumeMediaFixture()
			interruptedPrefix(t, dir, j)
			parts, _ := filepath.Glob(filepath.Join(dir, "media", "users", "*", "*.part"))
			switch scenario {
			case "url":
				j.URL = "https://video.twimg.com/new.mp4"
			case "corrupt":
				if err := os.WriteFile(parts[0], []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
			case "metadata":
				if err := os.WriteFile(parts[0]+".json", []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Range") != "bytes=0-5" || r.Header.Get("If-Range") != "" {
					t.Error("unsafe prefix reused", r.Header)
				}
				return mediaResponse(206, "bytes 0-5/6", `"v2"`, strings.NewReader("uvwxyz"), 6), nil
			})}
			path, size, hash, err := Download(context.Background(), c, dir, j, 6)
			if err != nil {
				t.Fatal(err)
			}
			assertMediaPayload(t, dir, path, size, hash, "uvwxyz")
		})
	}
}

func TestMediaDownloadUsesBoundedSequentialRanges(t *testing.T) {
	dir, j := t.TempDir(), resumeMediaFixture()
	payload := bytes.Repeat([]byte("x"), int(mediaChunkBytes)+17)
	calls := 0
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			t.Fatal(err)
		}
		if end-start+1 > mediaChunkBytes {
			t.Error("request is not bounded")
		}
		if calls > 1 && r.Header.Get("If-Range") != `"v1"` {
			t.Error("missing validator")
		}
		end = min(end, int64(len(payload))-1)
		return mediaResponse(206, fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)), `"v1"`, bytes.NewReader(payload[start:end+1]), end-start+1), nil
	})}
	path, size, hash, err := Download(context.Background(), c, dir, j, int64(len(payload)))
	if err != nil || calls != 2 {
		t.Fatal(calls, err)
	}
	assertMediaPayload(t, dir, path, size, hash, string(payload))
}

func TestMediaResumeNoValidatorAndInvalidBody(t *testing.T) {
	for _, etag := range []string{"", `W/"weak"`} {
		t.Run(etag, func(t *testing.T) {
			dir, j := t.TempDir(), resumeMediaFixture()
			c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return mediaResponse(200, "", etag, io.MultiReader(strings.NewReader("abc"), brokenMediaReader{io.ErrUnexpectedEOF}), 6), nil
			})}
			if _, _, _, err := Download(context.Background(), c, dir, j, 6); err == nil || strings.Contains(err.Error(), "已保留") {
				t.Fatal(err)
			}
			parts, _ := filepath.Glob(filepath.Join(dir, "media", "users", "*", ".download-*"))
			if len(parts) > 0 {
				t.Fatal("unsafe checkpoint kept", parts)
			}
		})
	}
	for _, scenario := range []string{"oversized", "too_long", "encoded", "html"} {
		t.Run(scenario, func(t *testing.T) {
			dir, j := t.TempDir(), resumeMediaFixture()
			interruptedPrefix(t, dir, j)
			c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				response := mediaResponse(200, "", `"v2"`, strings.NewReader("abcdef"), 6)
				switch scenario {
				case "oversized":
					response.ContentLength = 7
				case "too_long":
					response.ContentLength = 3
				case "encoded":
					response.Header.Set("Content-Encoding", "gzip")
				case "html":
					response.Header.Set("Content-Type", "text/html")
				}
				return response, nil
			})}
			if _, _, _, err := Download(context.Background(), c, dir, j, 6); err == nil {
				t.Fatal("invalid body accepted")
			}
			parts, _ := filepath.Glob(filepath.Join(dir, "media", "users", "*", ".download-*"))
			if len(parts) > 0 {
				t.Fatal("invalid checkpoint kept", parts)
			}
		})
	}
}

func TestMediaResumeCancellationKeepsVerifiedPrefix(t *testing.T) {
	dir, j := t.TempDir(), resumeMediaFixture()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		go func() { _, _ = writer.Write([]byte("abc")); cancel(); _ = writer.CloseWithError(context.Canceled) }()
		return mediaResponse(206, "bytes 0-5/6", `"v1"`, reader, 6), nil
	})}
	if _, _, _, err := Download(ctx, c, dir, j, 6); err == nil {
		t.Fatal("cancelled download completed")
	}
	c.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Range") != "bytes=3-5" {
			t.Error("cancelled prefix lost", r.Header)
		}
		return mediaResponse(206, "bytes 3-5/6", `"v1"`, strings.NewReader("def"), 3), nil
	})
	path, size, hash, err := Download(context.Background(), c, dir, j, 6)
	if err != nil {
		t.Fatal(err)
	}
	assertMediaPayload(t, dir, path, size, hash, "abcdef")
}

func TestMediaResumePreservesCheckpointOnHTTPFailure(t *testing.T) {
	dir, j := t.TempDir(), resumeMediaFixture()
	interruptedPrefix(t, dir, j)
	c := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return mediaResponse(503, "", "", strings.NewReader(""), 0), nil
	})}
	if _, _, _, err := Download(context.Background(), c, dir, j, 6); err == nil || !strings.Contains(err.Error(), "已保留 3 字节") {
		t.Fatal(err)
	}
	c.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("fixture network failure") })
	if _, _, _, err := Download(context.Background(), c, dir, j, 6); err == nil || !strings.Contains(err.Error(), "已保留 3 字节") {
		t.Fatal(err)
	}
}

func TestMediaResumeRealHTTPDisconnect(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("ETag", `"v1"`)
		if calls == 1 {
			w.Header().Set("Content-Range", "bytes 0-5/6")
			w.Header().Set("Content-Length", "6")
			w.WriteHeader(206)
			_, _ = io.WriteString(w, "abc") // net/http closes the short response connection.
			return
		}
		if r.Header.Get("Range") != "bytes=3-5" || r.Header.Get("If-Range") != `"v1"` {
			t.Error("actual HTTP request did not resume", r.Header)
		}
		w.Header().Set("Content-Range", "bytes 3-5/6")
		w.Header().Set("Content-Length", "3")
		w.WriteHeader(206)
		_, _ = io.WriteString(w, "def")
	}))
	defer server.Close()
	client := server.Client()
	transport := client.Transport.(*http.Transport).Clone()
	// All connections are routed to the local TLS fixture, including the allowlisted hostname.
	transport.TLSClientConfig.InsecureSkipVerify = true
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	client.Transport = transport
	defer client.CloseIdleConnections()
	client.Timeout = 5 * time.Second
	dir, j := t.TempDir(), resumeMediaFixture()
	if _, _, _, err := Download(context.Background(), client, dir, j, 6); err == nil || !strings.Contains(err.Error(), "已保留 3 字节") {
		t.Fatal(err)
	}
	path, size, hash, err := Download(context.Background(), client, dir, j, 6)
	if err != nil {
		t.Fatal(err)
	}
	assertMediaPayload(t, dir, path, size, hash, "abcdef")
	if calls != 2 {
		t.Fatal("unexpected network attempts", calls)
	}
}

func TestMediaResumeCompleteCheckpointPublishesWithoutRedownload(t *testing.T) {
	dir, j := t.TempDir(), resumeMediaFixture()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return mediaResponse(206, "bytes 0-5/6", `"v1"`, io.MultiReader(strings.NewReader("abcdef"), brokenMediaReader{io.ErrUnexpectedEOF}), 6), nil
	})}
	if _, _, _, err := Download(context.Background(), client, dir, j, 6); err == nil {
		t.Fatal("expected interrupted completion")
	}
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("completed checkpoint downloaded again")
		return nil, errors.New("unexpected request")
	})
	path, size, hash, err := Download(context.Background(), client, dir, j, 6)
	if err != nil {
		t.Fatal(err)
	}
	assertMediaPayload(t, dir, path, size, hash, "abcdef")
}

func TestMediaResumeFallsBackWhenPartialResponseHasNoValidator(t *testing.T) {
	dir, j := t.TempDir(), resumeMediaFixture()
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return mediaResponse(206, "bytes 0-2/6", "", strings.NewReader("abc"), 3), nil
		}
		if calls != 2 || r.Header.Get("Range") != "" {
			t.Error("unsafe ranged fallback", calls, r.Header)
		}
		return mediaResponse(200, "", "", strings.NewReader("abcdef"), 6), nil
	})}
	path, size, hash, err := Download(context.Background(), client, dir, j, 6)
	if err != nil {
		t.Fatal(err)
	}
	assertMediaPayload(t, dir, path, size, hash, "abcdef")
	if calls != 2 {
		t.Fatal(calls)
	}
}
