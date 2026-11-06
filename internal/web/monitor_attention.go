package web

import (
	"strings"

	"xm/internal/archive"
)

// Presentation only: scheduler decisions use structured Failure kinds/reasons.
// Known stored messages also let older databases display details without guessing
// that an ambiguous access error means a suspended or deleted account.
type MonitorAttention struct {
	Code       string `json:"code"`
	Label      string `json:"label"`
	Suggestion string `json:"suggestion"`
}

func monitorAttention(m Monitor) *MonitorAttention {
	msg := strings.TrimPrefix(m.LastError, "未能更新账号身份：")
	if m.LastStatus == "cancelled" && msg == "任务已停止；已入库内容保留" {
		return nil
	}
	if msg == "" && m.LastStatus != "failed" && m.LastStatus != "media_failed" && m.LastStatus != "incomplete" && m.LastStatus != "interrupted" && m.BaselineStatus != "blocked" {
		return nil
	}
	code, label, suggestion := "collection_failed", "采集异常，原因待确认", "打开采集详情查看错误；确认账号可访问后再重试。"
	switch {
	case strings.HasPrefix(msg, archive.AccountSuspendedMessage):
		code, label, suggestion = "suspended", "账号已被冻结", "X 已返回明确冻结标记；待账号解冻后，再开启自动采集。"
	case strings.HasPrefix(msg, archive.AccountNotFoundMessage), strings.HasPrefix(msg, archive.AccountLookupMissingMessage):
		code, label, suggestion = "username_unavailable", "账号未找到，可能已改名", "尚未确认注销或删除。可用“更新账号身份”核对当前用户名；只有数字用户 ID 一致时才延续原归档。"
		if m.UserID == "" {
			suggestion = "尚无可核对的数字用户 ID，无法自动确认改名；请在 X 核实账号及当前用户名。"
		} else if strings.Contains(msg, "自动") {
			suggestion = "已尝试自动核对，仍未确认改名。可在“更新账号身份”中填写当前用户名核对，或前往 X 确认账号状态。"
		}
	case strings.Contains(msg, "锁推账号"):
		code, label, suggestion = "protected", "锁推账号，无查看权限", "请确认采集用的登录账号已获准关注；在 X 能查看推文后，再开启自动采集。"
	case strings.Contains(msg, "Cookie") && (strings.Contains(msg, "失效") || strings.Contains(msg, "无效")), strings.Contains(msg, "采集账号受限"):
		code, label, suggestion = "auth", "采集登录态失效或受限", "请在浏览器处理采集账号的登录或验证，并重新导入 Cookie；这不代表目标账号被冻结。"
	case strings.Contains(msg, "限流"):
		code, label, suggestion = "rate", "X 请求限流", "等待限流冷却结束后自动重试，避免连续手动请求。"
	case strings.Contains(msg, "身份"), strings.Contains(msg, "用户 ID"):
		code, label, suggestion = "identity", "账号身份需核对", "使用“更新账号身份”核对当前用户名；系统不会将其他数字用户 ID 的内容混入原归档。"
	case strings.Contains(msg, "请重置断点"):
		code, label, suggestion = "checkpoint", "采集断点需要重置", "在管理菜单中重置断点并重试；已归档内容会保留。"
	case strings.Contains(msg, "不可用"), strings.Contains(msg, "拒绝访问"), strings.Contains(msg, "无权访问"):
		code, label, suggestion = "unavailable", "账号不可访问，原因未确认", "当前记录不足以区分冻结、改名或权限问题；可更新账号身份，或在 X 核实后重试。"
	case m.LastStatus == "media_failed":
		code, label, suggestion = "media", "媒体归档未完成", "正文已保留；请查看媒体错误和网络状态，后续任务会重试可恢复的媒体。"
	case m.LastStatus == "interrupted":
		code, label, suggestion = "interrupted", "上次任务被中断", "已归档内容保留；确认服务与自动采集已恢复后继续。"
	case m.LastStatus == "incomplete":
		code, label, suggestion = "incomplete", "本轮采集尚未完成", "已保存的内容与断点会保留；自动采集开启时，后续批次会继续。"
	}
	return &MonitorAttention{Code: code, Label: label, Suggestion: suggestion}
}
