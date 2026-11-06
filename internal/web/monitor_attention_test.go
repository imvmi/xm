package web

import (
	"strings"
	"testing"
	"xm/internal/archive"
)

func TestMonitorAttention(t *testing.T) {
	for _, tc := range []struct{ message, status, want string }{
		{archive.AccountSuspendedMessage, "failed", "suspended"},
		{"未能更新账号身份：" + archive.AccountSuspendedMessage, "failed", "suspended"},
		{archive.AccountNotFoundMessage, "failed", "username_unavailable"},
		{archive.AccountLookupMissingMessage + "；自动核对后用户名未变化，未确认改名", "failed", "username_unavailable"},
		{archive.AccountProtectedMessage, "failed", "protected"},
		{"当前账号无权查看该锁推账号的内容", "failed", "protected"},
		{"目标账号不可用或当前账号无权访问", "failed", "unavailable"},
		{"X UserByScreenName 拒绝访问：可能无查看权限、账号受限或需要浏览器验证", "failed", "unavailable"},
		{"采集账号受限或需要人工验证，请在浏览器处理", "failed", "auth"},
		{"Cookie 登录态失效，请重新导入", "failed", "auth"},
		{"X 请求限流，请等待后重试", "failed", "rate"},
		{"目标用户 ID 与绑定身份不符", "failed", "identity"},
		{"无法继续，请重置断点", "failed", "checkpoint"},
		{"下载失败", "media_failed", "media"},
		{"任务中断", "interrupted", "interrupted"},
		{incompleteCollectionMessage, "incomplete", "incomplete"},
		{"任务已停止；已入库内容保留", "cancelled", ""},
		{"", "bounded_complete", ""},
		{"未知错误", "failed", "collection_failed"},
	} {
		t.Run(tc.message, func(t *testing.T) {
			got := monitorAttention(Monitor{LastError: tc.message, LastStatus: tc.status})
			if tc.want == "" {
				if got != nil {
					t.Fatal(got)
				}
				return
			}
			if got == nil || got.Code != tc.want || got.Suggestion == "" {
				t.Fatal(got)
			}
		})
	}
	got := monitorAttention(Monitor{UserID: "42", LastError: archive.AccountNotFoundMessage + "；自动更新账号身份未成功：timeout"})
	if !strings.Contains(got.Suggestion, "已尝试自动核对") {
		t.Fatal(got)
	}
}
