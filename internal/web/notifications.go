package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"xm/internal/archive"
	"xm/internal/database"
)

func migrateNotifications(db *database.DB) error {
	if db.Dialect == "mysql" {
		return MigrateStore(db)
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS notification_targets(target TEXT PRIMARY KEY,ready INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS notification_seen(target TEXT NOT NULL,post_id TEXT NOT NULL,PRIMARY KEY(target,post_id));
CREATE TABLE IF NOT EXISTS notification_incidents(key TEXT PRIMARY KEY,kind TEXT NOT NULL,last_notice INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS notification_deliveries(id INTEGER PRIMARY KEY,event_id TEXT NOT NULL,channel_id TEXT NOT NULL,destination TEXT NOT NULL,payload TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'pending',attempts INTEGER NOT NULL DEFAULT 0,next_attempt INTEGER NOT NULL,created_at INTEGER NOT NULL,finished_at INTEGER NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT '',UNIQUE(event_id,channel_id));
CREATE INDEX IF NOT EXISTS notification_due ON notification_deliveries(status,next_attempt);`)
	return err
}
func notificationID() string {
	v := make([]byte, 16)
	if _, e := rand.Read(v); e != nil {
		panic(e)
	}
	return hex.EncodeToString(v)
}
func destination(ch archive.NotificationChannel) string {
	// Bind queued events to their recipient; a later edit must not redirect them.
	raw, _ := json.Marshal([]any{ch.Kind, ch.ChatID, ch.ThreadID, ch.BotToken, ch.WebhookURL, ch.BearerToken})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func enqueueNotification(tx *database.Tx, channels []archive.NotificationChannel, e NotificationEvent) error {
	if e.ID == "" {
		e.ID = notificationID()
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = time.Now().Unix()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	for _, ch := range channels {
		if e.Type != "test" && (!ch.Enabled || (e.Type == "new_posts" && !ch.Updates) || (e.Type != "new_posts" && !ch.Alerts)) {
			continue
		}
		if _, err = tx.InsertIgnore(`INSERT INTO notification_deliveries(event_id,channel_id,destination,payload,next_attempt,created_at) VALUES(?,?,?,?,?,?)`, "event_id", e.ID, ch.ID, destination(ch), string(raw), e.CreatedAt, e.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

// Seed existing archives once before collection; thereafter seen IDs and outbox
// insertion commit together. First historical collection stays silent.
func (a *App) prepareNotifications(m Monitor) error {
	tx, err := a.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.InsertIgnore("INSERT INTO notification_targets(target,ready) VALUES(?,?)", "target", m.Username, m.LastSuccess > 0)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		if _, err = tx.InsertIgnore("INSERT INTO notification_seen SELECT target,post_id FROM target_posts WHERE target=?", "post_id", m.Username); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (a *App) recordNotifications(cfg archive.Config, m Monitor, success, cancelled bool, kind string, failures int) error {
	tx, err := a.store.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	displayName := m.Username
	eName := tx.QueryRow("SELECT current_username FROM target_identities WHERE target=?", m.Username).Scan(&displayName)
	if eName != nil && !errors.Is(eName, sql.ErrNoRows) {
		return eName
	}

	var ready, count int
	if err = tx.QueryRow("SELECT ready FROM notification_targets WHERE target=?", m.Username).Scan(&ready); err != nil {
		return err
	}
	if err = tx.QueryRow(`SELECT COUNT(*) FROM target_posts t WHERE target=? AND NOT EXISTS(SELECT 1 FROM notification_seen s WHERE s.target=t.target AND s.post_id=t.post_id)`, m.Username).Scan(&count); err != nil {
		return err
	}
	if ready != 0 && count > 0 {
		if err = enqueueNotification(tx, cfg.NotificationChannels, NotificationEvent{Type: "new_posts", Target: displayName, Title: "XM · @" + displayName + " 有新动态", Message: fmt.Sprintf("本轮新增归档 %d 条动态，请登录 XM 查看正文和媒体。", count), Count: count}); err != nil {
			return err
		}
	}
	if _, err = tx.InsertIgnore("INSERT INTO notification_seen SELECT target,post_id FROM target_posts WHERE target=?", "post_id", m.Username); err != nil {
		return err
	}
	var current string
	if e := tx.QueryRow("SELECT current_username FROM target_identities WHERE target=?", m.Username).Scan(&current); e == nil && current != "" {
		var previous string
		key := "handle:" + m.Username
		e = tx.QueryRow("SELECT value FROM web_state WHERE `key`=?", key).Scan(&previous)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if previous == "" {
			previous = m.Username
		}
		if previous != current {
			if err = enqueueNotification(tx, cfg.NotificationChannels, NotificationEvent{Type: "username_changed", Target: current, Title: "XM · 监控账号已改名", Message: "@" + previous + " 已改为 @" + current + "，用户 ID 一致，继续使用原归档。"}); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(database.Upsert(tx.Dialect, "INSERT INTO web_state VALUES(?,?)", []string{"key"}, []string{"value"}), key, current); err != nil {
			return err
		}
	} else if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if success {
		if _, err = tx.Exec("UPDATE notification_targets SET ready=1 WHERE target=?", m.Username); err != nil {
			return err
		}
	}
	if !cancelled {
		switch kind {
		case "auth":
			err = notificationIncident(tx, cfg, "cookie", "auth", "", "XM · Cookie 失效", "X 登录状态失效或受限，自动采集已暂停。请在连接设置中更新 Cookie。")
		case "identity":
			err = notificationIncident(tx, cfg, "identity:"+m.Username, "identity", m.Username, "XM · 账号身份校验失败", "@"+m.Username+" 的身份无法确认或与绑定用户 ID 不符，监控已暂停，未接受不匹配账号的推文。请检查采集记录。")
		case "access":
			err = notificationIncident(tx, cfg, "access:"+m.Username, "access", m.Username, "XM · 账号无法访问", "@"+m.Username+" 无法访问，可能是权限变化、账号不可用或未获准关注。该账号监控已暂停，请检查。")
		case "rate":
			err = notificationIncident(tx, cfg, "rate", "rate", "", "XM · X 请求限流", "X 返回限流，采集已进入至少 15 分钟的冷却期。")
		case "media":
			err = notificationIncident(tx, cfg, "health:"+m.Username, "media_failed", m.Username, "XM · 媒体保存失败", "@"+m.Username+" 部分图片或视频尚未归档。请查看采集记录，检查网络、存储或文件限制；临时失败会在后续任务重试，永久失败需处理原因后修复。")
		case "transient":
			if failures >= 3 {
				err = notificationIncident(tx, cfg, "health:"+m.Username, "collection_failed", m.Username, "XM · 连续采集失败", fmt.Sprintf("@%s 已连续失败 %d 次。系统正在退避重试，请查看采集记录。", m.Username, failures))
			}
		}
		if err != nil {
			return err
		}
		if success {
			for _, key := range []string{"cookie", "rate", "access:" + m.Username, "health:" + m.Username, "identity:" + m.Username} {
				if err = notificationRecovery(tx, cfg, key, m.Username); err != nil {
					return err
				}
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.notifier.Wake()
	return nil
}
func notificationIncident(tx *database.Tx, cfg archive.Config, key, kind, target, title, message string) error {
	var old string
	var at int64
	err := tx.QueryRow("SELECT kind,last_notice FROM notification_incidents WHERE `key`=?", key).Scan(&old, &at)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := time.Now().Unix()
	if old == kind && now-at < 6*3600 {
		return nil
	}
	if _, err = tx.Exec(database.Upsert(tx.Dialect, "INSERT INTO notification_incidents VALUES(?,?,?)", []string{"key"}, []string{"kind", "last_notice"}), key, kind, now); err != nil {
		return err
	}
	return enqueueNotification(tx, cfg.NotificationChannels, NotificationEvent{Type: kind, Target: target, Title: title, Message: message})
}
func notificationRecovery(tx *database.Tx, cfg archive.Config, key, target string) error {
	var kind string
	err := tx.QueryRow("SELECT kind FROM notification_incidents WHERE `key`=?", key).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM notification_incidents WHERE `key`=?", key); err != nil {
		return err
	}
	message := "@" + target + " 已恢复正常采集和媒体保存。"
	if key == "cookie" {
		message = "更新 Cookie 后已成功采集，登录状态恢复正常。"
	} else if key == "rate" {
		message = "X 限流后已成功完成采集。"
	}
	return enqueueNotification(tx, cfg.NotificationChannels, NotificationEvent{Type: "recovered", Target: target, Title: "XM · 采集恢复", Message: message})
}

type Notifier struct {
	app       *App
	wake      chan struct{}
	mu        sync.Mutex
	lastError string
	send      func(context.Context, archive.Config, archive.NotificationChannel, NotificationEvent) error
}

func newNotifier(a *App) *Notifier {
	return &Notifier{app: a, wake: make(chan struct{}, 1), send: sendNotification}
}
func (n *Notifier) Wake() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}
func (n *Notifier) Error() string { n.mu.Lock(); defer n.mu.Unlock(); return n.lastError }
func (n *Notifier) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		worked, err := n.step(ctx)
		n.mu.Lock()
		if err != nil {
			n.lastError = "通知队列读写失败，请检查数据库连接与权限"
		} else {
			n.lastError = ""
		}
		n.mu.Unlock()
		if worked && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-n.wake:
		}
	}
}
func (n *Notifier) step(ctx context.Context) (bool, error) {
	if ctx.Err() != nil {
		return false, nil
	}
	var id int64
	var cid, dest, raw string
	var attempts int
	err := n.app.store.DB.QueryRow("SELECT id,channel_id,destination,payload,attempts FROM notification_deliveries WHERE status='pending' AND next_attempt<=? ORDER BY id LIMIT 1", time.Now().Unix()).Scan(&id, &cid, &dest, &raw, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var event NotificationEvent
	if err = json.Unmarshal([]byte(raw), &event); err != nil {
		return false, err
	}
	cfg := n.app.Config()
	var channel *archive.NotificationChannel
	for _, ch := range cfg.NotificationChannels {
		if ch.ID == cid {
			copy := ch
			channel = &copy
			break
		}
	}
	if channel == nil || destination(*channel) != dest || (event.Type != "test" && (!channel.Enabled || (event.Type == "new_posts" && !channel.Updates) || (event.Type != "new_posts" && !channel.Alerts))) {
		_, err = n.app.store.DB.Exec("UPDATE notification_deliveries SET status='cancelled',last_error='渠道已停用、移除或收件配置已变更',finished_at=? WHERE id=?", time.Now().Unix(), id)
		return true, err
	}
	err = n.send(ctx, cfg, *channel, event)
	if ctx.Err() != nil {
		return false, nil
	} // Leave pending for restart, not exhausted by shutdown.
	status, message := "sent", ""
	next := int64(0)
	attempts++
	if err != nil {
		status = "pending"
		message = "通知发送失败"
		delay := min(60<<(attempts-1), 3600)
		var de *deliveryError
		if errors.As(err, &de) {
			message = de.message
			delay = max(delay, de.retryAfter)
			if de.permanent {
				status = "failed"
			}
		}
		if attempts >= 5 {
			status = "failed"
		}
		next = time.Now().Unix() + int64(delay)
	}
	_, e := n.app.store.DB.Exec("UPDATE notification_deliveries SET status=?,attempts=?,last_error=?,next_attempt=?,finished_at=? WHERE id=?", status, attempts, message, next, time.Now().Unix(), id)
	// Retain pending work, and a bounded history of terminal deliveries.
	if e == nil {
		_, e = n.app.store.DB.Exec("DELETE FROM notification_deliveries WHERE status!='pending' AND id NOT IN (SELECT id FROM (SELECT id FROM notification_deliveries ORDER BY id DESC LIMIT 1000) AS retained)")
	}
	return true, e
}
