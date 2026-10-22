package archive

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"xm/internal/database"
)

type AdminCredentials struct {
	Username            string `json:"username"`
	Salt                []byte `json:"salt"`
	Hash                []byte `json:"hash"`
	ConfigurationLocked bool   `json:"configuration_locked"`
}

func (s *Store) AdminCredentials() (AdminCredentials, error) {
	var a AdminCredentials
	err := s.DB.QueryRow("SELECT username,password_salt,password_hash,configuration_locked FROM admin_accounts WHERE id=1").Scan(&a.Username, &a.Salt, &a.Hash, &a.ConfigurationLocked)
	return a, err
}

// SaveSettings commits settings and related state changes atomically. Updates
// must use the supplied transaction; any error rolls back the entire save.
func (s *Store) SaveSettings(c Config, updates ...func(*database.Tx) error) error {
	c, err := normalizeConfig("", c)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return errors.New("无法开始配置事务")
	}
	defer tx.Rollback()
	if err = saveSettings(tx, c); err != nil {
		return err
	}
	for _, update := range updates {
		if err = update(tx); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return errors.New("保存运行配置到数据库失败")
	}
	return nil
}
func saveSettings(tx *database.Tx, c Config) error {
	// The built-in local destination gives active_storage_id a real foreign key.
	profiles := append([]StorageProfile{{ID: "local", Name: "本地文件系统", Kind: "local", Timeout: 120}}, c.StorageProfiles...)
	ids := make([]any, 0, len(profiles))
	for i, p := range profiles {
		ids = append(ids, p.ID)
		_, err := tx.Exec(database.Upsert(tx.Dialect, `INSERT INTO storage_profiles(id,name,kind,endpoint,object_prefix,username,password,region,bucket,access_key,secret_key,session_token,path_style,use_proxy,timeout_seconds,position,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, []string{"id"}, []string{"name", "kind", "endpoint", "object_prefix", "username", "password", "region", "bucket", "access_key", "secret_key", "session_token", "path_style", "use_proxy", "timeout_seconds", "position", "updated_at"}), p.ID, p.Name, p.Kind, p.Endpoint, p.Prefix, p.Username, p.Password, p.Region, p.Bucket, p.AccessKey, p.SecretKey, p.SessionToken, p.PathStyle, p.UseProxy, p.Timeout, i, now())
		if err != nil {
			return errors.New("保存存储连接失败")
		}
	}
	// Keep the obsolete collection_timeout_seconds column populated for existing schemas.
	// Runtime deadlines use browser_max_age_seconds exclusively.
	_, err := tx.Exec(database.Upsert(tx.Dialect, `INSERT INTO system_settings(id,proxy,chrome_path,headless,request_timeout_seconds,collection_timeout_seconds,scroll_delay_seconds,scroll_jitter_seconds,max_scrolls,max_posts_per_run,page_budget,timeline_idle_seconds,known_boundary_posts,max_media_bytes,default_interval_seconds,queue_interval_seconds,request_interval_seconds,request_jitter_seconds,request_budget_mode,rate_limit_mode,browser_max_age_seconds,browser_max_tasks,media_retry_initial_seconds,media_retry_max_seconds,polling_paused,active_storage_id,archive_theme,timeline_page_size,video_default_muted,video_loop,media_preload_mode,image_cache_seconds,video_cache_seconds,sqlite_read_connections,updated_at) VALUES(1,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, []string{"id"}, []string{"proxy", "chrome_path", "headless", "request_timeout_seconds", "collection_timeout_seconds", "scroll_delay_seconds", "scroll_jitter_seconds", "max_scrolls", "max_posts_per_run", "page_budget", "timeline_idle_seconds", "known_boundary_posts", "max_media_bytes", "default_interval_seconds", "queue_interval_seconds", "request_interval_seconds", "request_jitter_seconds", "request_budget_mode", "rate_limit_mode", "browser_max_age_seconds", "browser_max_tasks", "media_retry_initial_seconds", "media_retry_max_seconds", "polling_paused", "active_storage_id", "archive_theme", "timeline_page_size", "video_default_muted", "video_loop", "media_preload_mode", "image_cache_seconds", "video_cache_seconds", "sqlite_read_connections", "updated_at"}), c.Proxy, c.ChromePath, c.Headless, c.RequestTimeout, 300, c.ScrollDelay, c.ScrollJitter, c.MaxScrolls, c.MaxPostsPerRun, c.PageBudget, c.TimelineIdleSeconds, c.KnownBoundaryPosts, c.MaxMediaBytes, c.DefaultInterval, c.QueueInterval, c.RequestInterval, c.RequestJitter, c.RequestBudgetMode, c.RateLimitMode, c.BrowserMaxAge, c.BrowserMaxTasks, c.MediaRetryInitial, c.MediaRetryMax, c.PollingPaused, c.ActiveStorage(), c.ArchiveTheme, c.TimelinePageSize, c.VideoMutedByDefault(), c.VideoLoop, c.MediaPreloadMode, c.ImageCacheSeconds, c.VideoCacheSeconds, c.SQLiteReadConnections, now())
	if err != nil {
		return errors.New("保存系统设置失败")
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	var referenced int
	if err = tx.QueryRow("SELECT COUNT(*) FROM media WHERE storage_id NOT IN ("+placeholders+")", ids...).Scan(&referenced); err != nil {
		return errors.New("检查存储引用失败")
	}
	if referenced != 0 {
		return errors.New("存储连接仍被已归档媒体引用，不能移除")
	}
	if _, err = tx.Exec("DELETE FROM storage_profiles WHERE id NOT IN ("+placeholders+")", ids...); err != nil {
		return errors.New("清理存储连接失败")
	}
	// Delivery history intentionally retains channel IDs after a channel is removed.
	if _, err = tx.Exec("DELETE FROM notification_channels"); err != nil {
		return errors.New("更新通知渠道失败")
	}
	for i, ch := range c.NotificationChannels {
		_, err = tx.Exec(`INSERT INTO notification_channels(id,name,kind,enabled,updates,alerts,use_proxy,bot_token,chat_id,thread_id,webhook_url,bearer_token,position,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, ch.ID, ch.Name, ch.Kind, ch.Enabled, ch.Updates, ch.Alerts, ch.UseProxy, ch.BotToken, ch.ChatID, ch.ThreadID, ch.WebhookURL, ch.BearerToken, i, now())
		if err != nil {
			return errors.New("保存通知渠道失败")
		}
	}
	return nil
}
func (s *Store) LoadSettings(base Config) (Config, error) {
	c := DefaultConfig()
	c.DataDir, c.Database = base.DataDir, base.Database
	// Read all tables from one snapshot; a concurrent save cannot mix versions.
	tx, err := s.DB.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return c, errors.New("读取数据库配置失败")
	}
	defer tx.Rollback()
	err = tx.QueryRow(`SELECT proxy,chrome_path,headless,request_timeout_seconds,scroll_delay_seconds,scroll_jitter_seconds,max_scrolls,max_posts_per_run,page_budget,timeline_idle_seconds,known_boundary_posts,max_media_bytes,default_interval_seconds,queue_interval_seconds,request_interval_seconds,request_jitter_seconds,request_budget_mode,rate_limit_mode,browser_max_age_seconds,browser_max_tasks,media_retry_initial_seconds,media_retry_max_seconds,polling_paused,active_storage_id,archive_theme,timeline_page_size,video_default_muted,video_loop,media_preload_mode,image_cache_seconds,video_cache_seconds,sqlite_read_connections FROM system_settings WHERE id=1`).Scan(&c.Proxy, &c.ChromePath, &c.Headless, &c.RequestTimeout, &c.ScrollDelay, &c.ScrollJitter, &c.MaxScrolls, &c.MaxPostsPerRun, &c.PageBudget, &c.TimelineIdleSeconds, &c.KnownBoundaryPosts, &c.MaxMediaBytes, &c.DefaultInterval, &c.QueueInterval, &c.RequestInterval, &c.RequestJitter, &c.RequestBudgetMode, &c.RateLimitMode, &c.BrowserMaxAge, &c.BrowserMaxTasks, &c.MediaRetryInitial, &c.MediaRetryMax, &c.PollingPaused, &c.MediaStorage, &c.ArchiveTheme, &c.TimelinePageSize, &c.VideoDefaultMuted, &c.VideoLoop, &c.MediaPreloadMode, &c.ImageCacheSeconds, &c.VideoCacheSeconds, &c.SQLiteReadConnections)
	if errors.Is(err, sql.ErrNoRows) {
		var n int
		if err = tx.QueryRow("SELECT COUNT(*) FROM admin_accounts").Scan(&n); err != nil {
			return c, errors.New("读取初始化状态失败")
		}
		if n != 0 {
			return c, errors.New("数据库运行配置缺失，请恢复备份")
		}
		return c, nil
	}
	if err != nil {
		return c, errors.New("读取系统设置失败")
	}
	rows, err := tx.Query(`SELECT id,name,kind,endpoint,object_prefix,username,password,region,bucket,access_key,secret_key,session_token,path_style,use_proxy,timeout_seconds FROM storage_profiles WHERE id<>'local' ORDER BY position,id`)
	if err != nil {
		return c, errors.New("读取存储连接失败")
	}
	for rows.Next() {
		var p StorageProfile
		if err = rows.Scan(&p.ID, &p.Name, &p.Kind, &p.Endpoint, &p.Prefix, &p.Username, &p.Password, &p.Region, &p.Bucket, &p.AccessKey, &p.SecretKey, &p.SessionToken, &p.PathStyle, &p.UseProxy, &p.Timeout); err != nil {
			rows.Close()
			return c, errors.New("读取存储连接失败")
		}
		c.StorageProfiles = append(c.StorageProfiles, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return c, errors.New("读取存储连接失败")
	}
	rows, err = tx.Query(`SELECT id,name,kind,enabled,updates,alerts,use_proxy,bot_token,chat_id,thread_id,webhook_url,bearer_token FROM notification_channels ORDER BY position,id`)
	if err != nil {
		return c, errors.New("读取通知渠道失败")
	}
	for rows.Next() {
		var ch NotificationChannel
		if err = rows.Scan(&ch.ID, &ch.Name, &ch.Kind, &ch.Enabled, &ch.Updates, &ch.Alerts, &ch.UseProxy, &ch.BotToken, &ch.ChatID, &ch.ThreadID, &ch.WebhookURL, &ch.BearerToken); err != nil {
			rows.Close()
			return c, errors.New("读取通知渠道失败")
		}
		c.NotificationChannels = append(c.NotificationChannels, ch)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return c, errors.New("读取通知渠道失败")
	}
	return normalizeConfig("", c)
}

// Initialize claims the singleton admin and saves all configuration atomically.
func (s *Store) Initialize(c Config, admin AdminCredentials) error {
	c, err := normalizeConfig("", c)
	if err != nil {
		return err
	}
	if len(admin.Username) < 3 || len(admin.Username) > 32 || len(admin.Salt) != 32 || len(admin.Hash) != 32 || !admin.ConfigurationLocked {
		return errors.New("管理员凭据格式无效")
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return errors.New("无法开始初始化事务")
	}
	defer tx.Rollback()
	_, err = tx.Exec("INSERT INTO admin_accounts(id,username,password_salt,password_hash,configuration_locked,created_at) VALUES(1,?,?,?,?,?)", admin.Username, admin.Salt, admin.Hash, admin.ConfigurationLocked, now())
	if err != nil {
		return errors.New("管理员已存在或无法写入，初始化未完成")
	}
	if err = saveSettings(tx, c); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return errors.New("初始化提交未确认，请重启检查数据库状态")
	}
	return nil
}
