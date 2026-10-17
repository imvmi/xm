package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"xm/internal/archive"
)

type NotificationEvent struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Target    string `json:"target,omitempty"`
	Title     string `json:"title"`
	Message   string `json:"message"`
	Count     int    `json:"count,omitempty"`
	CreatedAt int64  `json:"created_at"`
}
type deliveryError struct {
	message    string
	permanent  bool
	retryAfter int
}

func (e *deliveryError) Error() string { return e.message }
func sendNotification(ctx context.Context, cfg archive.Config, ch archive.NotificationChannel, event NotificationEvent) error {
	proxy := ""
	if ch.UseProxy {
		proxy = cfg.Proxy
	}
	client, err := archive.HTTPClient(proxy, 20*time.Second)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	return sendNotificationHTTP(ctx, client, ch, event)
}
func sendNotificationHTTP(ctx context.Context, client *http.Client, ch archive.NotificationChannel, event NotificationEvent) error {
	// No redirect may forward credentials or private event metadata elsewhere.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	endpoint := ch.WebhookURL
	var payload any = event
	if ch.Kind == "telegram" {
		endpoint = "https://api.telegram.org/bot" + ch.BotToken + "/sendMessage"
		p := map[string]any{"chat_id": ch.ChatID, "text": event.Title + "\n\n" + event.Message, "link_preview_options": map[string]bool{"is_disabled": true}, "protect_content": true}
		if ch.ThreadID > 0 {
			p["message_thread_id"] = ch.ThreadID
		}
		payload = p
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return errors.New("通知内容编码失败")
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(b))
	if err != nil {
		return &deliveryError{message: "通知地址无效", permanent: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "XM-Notifier/1")
	if ch.Kind == "webhook" {
		req.Header.Set("X-XM-Event-ID", event.ID)
		if ch.BearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+ch.BearerToken)
		}
	}
	resp, err := c.Do(req)
	if err != nil {
		return &deliveryError{message: "通知网络连接失败或超时，请检查代理与网络"}
	}
	defer resp.Body.Close()
	code := resp.StatusCode
	retryAfter := 0
	if raw := resp.Header.Get("Retry-After"); raw != "" {
		retryAfter, _ = strconv.Atoi(raw)
		if retryAfter == 0 {
			if t, e := http.ParseTime(raw); e == nil {
				retryAfter = int(time.Until(t).Seconds())
			}
		}
	}
	if ch.Kind == "telegram" {
		var result struct {
			OK         bool `json:"ok"`
			Code       int  `json:"error_code"`
			Parameters struct {
				RetryAfter int `json:"retry_after"`
			} `json:"parameters"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result)
		if err != nil {
			return &deliveryError{message: fmt.Sprintf("Telegram 返回无法识别的响应（HTTP %d）", code), permanent: code >= 400 && code < 500 && code != 429}
		}
		if code >= 200 && code < 300 && result.OK {
			return nil
		}
		if result.Code != 0 {
			code = result.Code
		}
		retryAfter = max(retryAfter, result.Parameters.RetryAfter)
	} else if code >= 200 && code < 300 {
		return nil
	}
	msg := fmt.Sprintf("通知服务返回 HTTP/API %d", code)
	switch code {
	case 400:
		msg += "，请检查 Chat ID、Topic ID 或消息格式"
	case 401:
		msg += "，请检查 Bot Token 或认证信息"
	case 403:
		msg += "，请先向机器人发送 /start，并检查发送权限"
	case 429:
		msg += "，已按限流要求延迟重试"
	}
	// Never include provider descriptions: they may echo tokens or URLs.
	return &deliveryError{message: msg, permanent: code >= 300 && code < 500 && code != 408 && code != 429, retryAfter: max(0, retryAfter)}
}
