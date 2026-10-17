package archive

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

type NotificationChannel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Enabled     bool   `json:"enabled"`
	Updates     bool   `json:"updates"`
	Alerts      bool   `json:"alerts"`
	UseProxy    bool   `json:"use_proxy"`
	BotToken    string `json:"bot_token"`
	ChatID      string `json:"chat_id"`
	ThreadID    int64  `json:"thread_id"`
	WebhookURL  string `json:"webhook_url"`
	BearerToken string `json:"bearer_token"`
}

var botTokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

func (c NotificationChannel) Validate() error {
	if !storageID.MatchString(c.ID) || strings.TrimSpace(c.Name) == "" || len(c.Name) > 100 {
		return errors.New("通知渠道名称或标识无效")
	}
	if c.Kind == "telegram" {
		if !botTokenPattern.MatchString(c.BotToken) || len(c.BotToken) > 256 || strings.TrimSpace(c.ChatID) == "" || len(c.ChatID) > 128 || c.ThreadID < 0 {
			return errors.New("请填写有效的 Bot Token、Chat ID 和非负的 Topic ID")
		}
	} else if c.Kind == "webhook" {
		u, e := url.Parse(c.WebhookURL)
		if e != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
			return errors.New("Webhook 必须为 HTTP(S) URL，不允许内嵌用户名密码或片段")
		}
		if strings.ContainsAny(c.BearerToken, "\r\n") {
			return errors.New("Bearer Token 不能包含换行")
		}
	} else {
		return errors.New("通知渠道仅支持 Telegram 或通用 Webhook")
	}
	return nil
}
func (c Config) ValidateNotifications() error {
	if len(c.NotificationChannels) > 16 {
		return errors.New("最多配置 16 个通知渠道")
	}
	seen := map[string]bool{}
	for _, v := range c.NotificationChannels {
		if err := v.Validate(); err != nil {
			return err
		}
		if seen[v.ID] {
			return errors.New("通知渠道 ID 重复")
		}
		seen[v.ID] = true
	}
	return nil
}
