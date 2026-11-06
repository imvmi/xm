package web

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"xm/internal/archive"
)

func TestOutstandingMediaMessage(t *testing.T) {
	for _, tc := range []struct {
		status string
		next   int64
		want   string
	}{
		{"pending", 0, "等待下载"},
		{"failed", 1, "等待下次任务重试"},
		{"failed", time.Now().Add(time.Hour).Unix(), "等待重试，最早"},
		{"permanent", 0, "永久失败，不会自动重试"},
	} {
		issue := &archive.MediaIssue{PostID: strings.Repeat("1", 64), Kind: "video", Status: tc.status, NextAttempt: tc.next, Error: strings.Repeat("错误信息", 100)}
		message := outstandingMediaMessage(3, issue)
		if !strings.Contains(message, tc.want) || !strings.Contains(message, "仍有 3 个媒体") || !strings.Contains(message, "的视频") || len(message) > 500 || !utf8.ValidString(message) {
			t.Fatal("invalid media diagnostic", message)
		}
	}
	if message := outstandingMediaMessage(1, &archive.MediaIssue{PostID: "100", Kind: "photo"}); !strings.Contains(message, "尚未记录下载错误") {
		t.Fatal(message)
	}
	if message := outstandingMediaMessage(1, nil); !strings.Contains(message, "请检查媒体状态") {
		t.Fatal(message)
	}
}
