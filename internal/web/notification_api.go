package web

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"xm/internal/archive"
)

type notificationChannelView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Enabled     bool   `json:"enabled"`
	Updates     bool   `json:"updates"`
	Alerts      bool   `json:"alerts"`
	UseProxy    bool   `json:"use_proxy"`
	ChatID      string `json:"chat_id"`
	ThreadID    int64  `json:"thread_id"`
	BotTokenSet bool   `json:"bot_token_configured"`
	WebhookSet  bool   `json:"webhook_configured"`
	BearerSet   bool   `json:"bearer_configured"`
}
type notificationDeliveryView struct {
	ID          int64             `json:"id"`
	ChannelID   string            `json:"channel_id"`
	Status      string            `json:"status"`
	Attempts    int               `json:"attempts"`
	NextAttempt int64             `json:"next_attempt"`
	CreatedAt   int64             `json:"created_at"`
	Error       string            `json:"error"`
	Event       NotificationEvent `json:"event"`
}

func (a *App) notificationSettings(w http.ResponseWriter, r *http.Request) {
	channels := []notificationChannelView{}
	for _, ch := range a.Config().NotificationChannels {
		channels = append(channels, notificationChannelView{ch.ID, ch.Name, ch.Kind, ch.Enabled, ch.Updates, ch.Alerts, ch.UseProxy, ch.ChatID, ch.ThreadID, ch.BotToken != "", ch.WebhookURL != "", ch.BearerToken != ""})
	}
	rows, err := a.store.DB.Query("SELECT id,channel_id,status,attempts,next_attempt,created_at,last_error,payload FROM notification_deliveries ORDER BY id DESC LIMIT 30")
	if err != nil {
		problem(w, 500, "读取通知记录失败")
		return
	}
	defer rows.Close()
	list := []notificationDeliveryView{}
	for rows.Next() {
		var d notificationDeliveryView
		var raw string
		if err = rows.Scan(&d.ID, &d.ChannelID, &d.Status, &d.Attempts, &d.NextAttempt, &d.CreatedAt, &d.Error, &raw); err != nil {
			problem(w, 500, "读取通知记录失败")
			return
		}
		if err = json.Unmarshal([]byte(raw), &d.Event); err != nil {
			problem(w, 500, "通知记录内容损坏")
			return
		}
		list = append(list, d)
	}
	if rows.Err() != nil {
		problem(w, 500, "读取通知记录失败")
		return
	}
	respond(w, 200, map[string]any{"channels": channels, "deliveries": list, "worker_error": a.notifier.Error()})
}
func (a *App) saveNotificationChannel(w http.ResponseWriter, r *http.Request) {
	var v struct {
		archive.NotificationChannel
		ClearBearer bool `json:"clear_bearer"`
	}
	if !decode(w, r, &v) {
		return
	}
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	cfg := a.Config()
	ch := v.NotificationChannel
	ch.Name = strings.TrimSpace(ch.Name)
	ch.ChatID = strings.TrimSpace(ch.ChatID)
	ch.BotToken = strings.TrimSpace(ch.BotToken)
	ch.WebhookURL = strings.TrimSpace(ch.WebhookURL)
	idx := -1
	if ch.ID != "" {
		for i, old := range cfg.NotificationChannels {
			if old.ID == ch.ID {
				idx = i
				if ch.Kind != old.Kind {
					problem(w, 400, "修改渠道类型请新增渠道")
					return
				}
				if ch.BotToken == "" {
					ch.BotToken = old.BotToken
				}
				if ch.WebhookURL == "" {
					ch.WebhookURL = old.WebhookURL
				}
				if ch.BearerToken == "" {
					ch.BearerToken = old.BearerToken
				}
				break
			}
		}
		if idx < 0 {
			problem(w, 404, "通知渠道不存在")
			return
		}
	} else {
		ch.ID = "notify_" + notificationID()
	}
	if v.ClearBearer {
		ch.BearerToken = ""
	}
	if ch.Kind == "telegram" {
		ch.WebhookURL = ""
		ch.BearerToken = ""
	} else {
		ch.BotToken = ""
		ch.ChatID = ""
		ch.ThreadID = 0
	}
	if err := ch.Validate(); err != nil {
		problem(w, 400, err.Error())
		return
	}
	if idx < 0 {
		cfg.NotificationChannels = append(cfg.NotificationChannels, ch)
	} else {
		cfg.NotificationChannels[idx] = ch
	}
	if err := cfg.ValidateNotifications(); err != nil {
		problem(w, 400, err.Error())
		return
	}
	if err := a.saveConfig(cfg); err != nil {
		problem(w, 500, "保存通知配置失败")
		return
	}
	// If collection is already blocked, a newly enabled channel must not wait for
	// another collection to learn about the incident. Stable ID avoids save spam.
	if ch.Enabled && ch.Alerts && state(a.store.DB, "cookie_blocked") != "" {
		generation := state(a.store.DB, "cookie_imported")
		tx, err := a.store.DB.Begin()
		if err != nil {
			problem(w, 500, "配置已保存，但创建当前异常提醒失败")
			return
		}
		defer tx.Rollback()
		e := NotificationEvent{ID: "cookie-existing:" + ch.ID + ":" + generation + ":" + destination(ch), Type: "auth", Title: "XM · Cookie 失效", Message: "当前 X 登录状态失效或受限，采集已暂停。请在连接设置中更新 Cookie。"}
		err = enqueueNotification(tx, []archive.NotificationChannel{ch}, e)
		if err == nil {
			_, err = tx.Exec("INSERT OR IGNORE INTO notification_incidents VALUES('cookie','auth',?)", time.Now().Unix())
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			problem(w, 500, "配置已保存，但创建当前异常提醒失败")
			return
		}
	}
	a.notifier.Wake()
	respond(w, 200, map[string]string{"id": ch.ID})
}
func (a *App) deleteNotificationChannel(w http.ResponseWriter, r *http.Request) {
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	cfg := a.Config()
	found := false
	keep := []archive.NotificationChannel{}
	for _, ch := range cfg.NotificationChannels {
		if ch.ID == r.PathValue("id") {
			found = true
		} else {
			keep = append(keep, ch)
		}
	}
	if !found {
		problem(w, 404, "通知渠道不存在")
		return
	}
	cfg.NotificationChannels = keep
	if err := a.saveConfig(cfg); err != nil {
		problem(w, 500, "移除通知渠道失败")
		return
	}
	a.notifier.Wake()
	respond(w, 200, map[string]bool{"ok": true})
}
func (a *App) testNotificationChannel(w http.ResponseWriter, r *http.Request) {
	a.changeMu.Lock()
	defer a.changeMu.Unlock()
	for _, ch := range a.Config().NotificationChannels {
		if ch.ID == r.PathValue("id") {
			// Explicit tests work for disabled channels. Persist status just like real events.
			tx, err := a.store.DB.Begin()
			if err != nil {
				problem(w, 500, "创建测试通知失败")
				return
			}
			defer tx.Rollback()
			e := NotificationEvent{Type: "test", Title: "XM · 通知测试", Message: "这是一条测试消息。收到此消息表示 XM 通知渠道配置成功。"}
			if err = enqueueNotification(tx, []archive.NotificationChannel{ch}, e); err == nil {
				err = tx.Commit()
			}
			if err != nil {
				problem(w, 500, "创建测试通知失败")
				return
			}
			a.notifier.Wake()
			respond(w, 200, map[string]string{"message": "测试通知已加入队列，请查看发送记录和接收端"})
			return
		}
	}
	problem(w, 404, "通知渠道不存在")
}
func (a *App) retryNotification(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		problem(w, 400, "通知记录无效")
		return
	}
	result, err := a.store.DB.Exec("UPDATE notification_deliveries SET status='pending',attempts=0,next_attempt=?,last_error='' WHERE id=? AND status='failed'", time.Now().Unix(), id)
	if err != nil {
		problem(w, 500, "重试通知失败")
		return
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		problem(w, 409, "仅可重试发送失败的记录")
		return
	}
	a.notifier.Wake()
	respond(w, 200, map[string]bool{"ok": true})
}
