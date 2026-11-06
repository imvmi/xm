package web

import (
	"fmt"
	"strings"
	"time"

	"xm/internal/archive"
)

func outstandingMediaMessage(count int, issue *archive.MediaIssue) string {
	message := fmt.Sprintf("仍有 %d 个媒体未完成归档", count)
	if issue == nil {
		return message + "，请检查媒体状态"
	}
	kind := "视频"
	if issue.Kind == "photo" {
		kind = "图片"
	}
	status := "等待下载"
	switch issue.Status {
	case "permanent":
		status = "永久失败，不会自动重试"
	case "failed":
		status = "等待下次任务重试"
		if issue.NextAttempt > time.Now().Unix() {
			status = "等待重试，最早 " + time.Unix(issue.NextAttempt, 0).UTC().Format("01/02 15:04 UTC")
		}
	}
	reason := strings.TrimSpace(issue.Error)
	if reason == "" {
		reason = "尚未记录下载错误"
	}
	// Keep the useful diagnostic inside the run message's existing size limit.
	if chars := []rune(reason); len(chars) > 50 {
		reason = string(chars[:50]) + "…"
	}
	return fmt.Sprintf("%s；示例：推文 %s 的%s，%s；原因：%s", message, issue.PostID, kind, status, reason)
}
