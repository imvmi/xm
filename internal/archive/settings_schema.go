package archive

import (
	"strings"

	"xm/internal/database"
)

const systemSettingsColumns = "id,proxy,chrome_path,browser_mode,cdp_host,cdp_port,headless,request_timeout_seconds,collection_timeout_seconds,scroll_delay_seconds,scroll_jitter_seconds,max_scrolls,max_posts_per_run,page_budget,timeline_idle_seconds,known_boundary_posts,max_media_bytes,default_interval_seconds,queue_interval_seconds,request_interval_seconds,request_jitter_seconds,request_budget_mode,rate_limit_mode,browser_max_age_seconds,browser_max_tasks,media_retry_initial_seconds,media_retry_max_seconds,polling_paused,active_storage_id,archive_theme,timeline_page_size,video_default_muted,video_loop,video_auto_next,video_timeline_autoplay,video_timeline_preload,media_preload_mode,image_cache_seconds,video_cache_seconds,sqlite_read_connections,updated_at"

const systemSettingsDDL = `CREATE TABLE IF NOT EXISTS system_settings(id INTEGER PRIMARY KEY CHECK(id=1),proxy TEXT NOT NULL,chrome_path TEXT NOT NULL,browser_mode VARCHAR(16) NOT NULL DEFAULT 'local' CHECK(browser_mode IN ('local','cdp')),cdp_host VARCHAR(255) NOT NULL DEFAULT '127.0.0.1',cdp_port INTEGER NOT NULL DEFAULT 9222 CHECK(cdp_port BETWEEN 1 AND 65535),headless BOOLEAN NOT NULL,request_timeout_seconds INTEGER NOT NULL CHECK(request_timeout_seconds>0),collection_timeout_seconds INTEGER NOT NULL CHECK(collection_timeout_seconds>0),scroll_delay_seconds INTEGER NOT NULL CHECK(scroll_delay_seconds>0),scroll_jitter_seconds INTEGER NOT NULL DEFAULT 0 CHECK(scroll_jitter_seconds>=0),max_scrolls INTEGER NOT NULL CHECK(max_scrolls>0),max_posts_per_run INTEGER NOT NULL DEFAULT 1000 CHECK(max_posts_per_run BETWEEN 1 AND 10000),page_budget INTEGER NOT NULL DEFAULT 15 CHECK(page_budget BETWEEN 1 AND 100),timeline_idle_seconds INTEGER NOT NULL DEFAULT 24 CHECK(timeline_idle_seconds BETWEEN 1 AND 300),known_boundary_posts INTEGER NOT NULL DEFAULT 8 CHECK(known_boundary_posts BETWEEN 1 AND 100),max_media_bytes BIGINT NOT NULL CHECK(max_media_bytes>0),default_interval_seconds INTEGER NOT NULL CHECK(default_interval_seconds BETWEEN 3600 AND 604800),queue_interval_seconds INTEGER NOT NULL DEFAULT 0 CHECK(queue_interval_seconds BETWEEN 0 AND 3600),request_interval_seconds INTEGER NOT NULL DEFAULT 5 CHECK(request_interval_seconds>=5),request_jitter_seconds INTEGER NOT NULL DEFAULT 0 CHECK(request_jitter_seconds>=0),request_budget_mode VARCHAR(16) NOT NULL DEFAULT 'balanced' CHECK(request_budget_mode IN ('conservative','balanced','fast','turbo')),rate_limit_mode VARCHAR(16) NOT NULL DEFAULT 'balanced' CHECK(rate_limit_mode IN ('conservative','balanced','fast')),browser_max_age_seconds INTEGER NOT NULL DEFAULT 21600 CHECK(browser_max_age_seconds BETWEEN 300 AND 86400),browser_max_tasks INTEGER NOT NULL DEFAULT 100 CHECK(browser_max_tasks BETWEEN 1 AND 1000),media_retry_initial_seconds INTEGER NOT NULL DEFAULT 300 CHECK(media_retry_initial_seconds BETWEEN 60 AND 86400),media_retry_max_seconds INTEGER NOT NULL DEFAULT 86400 CHECK(media_retry_max_seconds BETWEEN 60 AND 604800),polling_paused BOOLEAN NOT NULL,active_storage_id VARCHAR(64) NOT NULL,archive_theme VARCHAR(16) NOT NULL DEFAULT 'system' CHECK(archive_theme IN ('system','light','dark')),timeline_page_size INTEGER NOT NULL DEFAULT 24 CHECK(timeline_page_size BETWEEN 10 AND 1000),video_default_muted BOOLEAN NOT NULL DEFAULT 1,video_loop BOOLEAN NOT NULL DEFAULT 0,video_auto_next BOOLEAN NOT NULL DEFAULT 0,video_timeline_autoplay BOOLEAN NOT NULL DEFAULT 0,video_timeline_preload BOOLEAN NOT NULL DEFAULT 0,media_preload_mode VARCHAR(16) NOT NULL DEFAULT 'standard',image_cache_seconds INTEGER NOT NULL DEFAULT 604800 CHECK(image_cache_seconds BETWEEN 3600 AND 2592000),video_cache_seconds INTEGER NOT NULL DEFAULT 600 CHECK(video_cache_seconds BETWEEN 60 AND 86400),sqlite_read_connections INTEGER NOT NULL DEFAULT 4 CHECK(sqlite_read_connections BETWEEN 1 AND 16),updated_at VARCHAR(64) NOT NULL,FOREIGN KEY(active_storage_id) REFERENCES storage_profiles(id))`

// Shared logical schema: SQLite and MySQL use the same keys and constraints.
// Per-service credentials are columns, not one opaque configuration document.
var settingsSchema = []string{
	`CREATE TABLE IF NOT EXISTS avatar_sources(user_id VARCHAR(64) PRIMARY KEY,source_url TEXT NOT NULL,updated_at VARCHAR(64) NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS api_credentials(id VARCHAR(64) PRIMARY KEY,name VARCHAR(100) NOT NULL,description TEXT NOT NULL,secret_hash VARCHAR(64) NOT NULL,secret_tail VARCHAR(8) NOT NULL,scopes TEXT NOT NULL,accounts TEXT NOT NULL,all_accounts BOOLEAN NOT NULL,enabled BOOLEAN NOT NULL,revoked_at BIGINT NOT NULL,expires_at BIGINT NOT NULL,created_at BIGINT NOT NULL,last_used_at BIGINT NOT NULL,request_limit INTEGER NOT NULL,task_limit INTEGER NOT NULL,queue_limit INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS api_idempotency(id VARCHAR(64) PRIMARY KEY,credential_id VARCHAR(64) NOT NULL,request_hash VARCHAR(64) NOT NULL,status_code INTEGER NOT NULL,response TEXT NOT NULL,expires_at BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS api_audit(id VARCHAR(64) PRIMARY KEY,credential_id VARCHAR(64) NOT NULL,action VARCHAR(255) NOT NULL,status_code INTEGER NOT NULL,created_at BIGINT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS media_local_copies(copy_id VARCHAR(64) PRIMARY KEY,post_id VARCHAR(64) NOT NULL,media_id VARCHAR(128) NOT NULL,local_path TEXT NOT NULL,storage_id VARCHAR(64) NOT NULL,object_key TEXT NOT NULL,bytes BIGINT NOT NULL,sha256 VARCHAR(64) NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS account_preferences(target VARCHAR(64) PRIMARY KEY,show_in_timeline BOOLEAN NOT NULL DEFAULT 1)`,
	`CREATE TABLE IF NOT EXISTS request_budgets(scope VARCHAR(64) PRIMARY KEY,state TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS collection_progress(target VARCHAR(64) PRIMARY KEY,state TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS run_metrics(run_id BIGINT PRIMARY KEY,new_posts INTEGER NOT NULL DEFAULT 0,posts_read INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE IF NOT EXISTS cookie_sources(id INTEGER PRIMARY KEY CHECK(id=1),endpoint TEXT NOT NULL,bearer_token TEXT NOT NULL,auto_refresh BOOLEAN NOT NULL,use_proxy BOOLEAN NOT NULL,updated_at VARCHAR(64) NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS schema_meta(id INTEGER PRIMARY KEY CHECK(id=1),version INTEGER NOT NULL,updated_at VARCHAR(64) NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS admin_accounts(id INTEGER PRIMARY KEY CHECK(id=1),username VARCHAR(32) NOT NULL UNIQUE,password_salt BLOB NOT NULL CHECK(length(password_salt)=32),password_hash BLOB NOT NULL CHECK(length(password_hash)=32),configuration_locked BOOLEAN NOT NULL CHECK(configuration_locked=1),created_at VARCHAR(64) NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS storage_profiles(id VARCHAR(64) PRIMARY KEY,name VARCHAR(100) NOT NULL,kind VARCHAR(16) NOT NULL CHECK(kind IN ('local','webdav','s3','smb')),endpoint TEXT NOT NULL,object_prefix TEXT NOT NULL,username TEXT NOT NULL,password TEXT NOT NULL,region TEXT NOT NULL,bucket TEXT NOT NULL,access_key TEXT NOT NULL,secret_key TEXT NOT NULL,session_token TEXT NOT NULL,path_style BOOLEAN NOT NULL,use_proxy BOOLEAN NOT NULL,timeout_seconds INTEGER NOT NULL CHECK(timeout_seconds BETWEEN 5 AND 3600),position INTEGER NOT NULL,updated_at VARCHAR(64) NOT NULL)`,
	systemSettingsDDL,
	`CREATE TABLE IF NOT EXISTS notification_channels(id VARCHAR(64) PRIMARY KEY,name VARCHAR(100) NOT NULL,kind VARCHAR(16) NOT NULL CHECK(kind IN ('telegram','webhook')),enabled BOOLEAN NOT NULL,updates BOOLEAN NOT NULL,alerts BOOLEAN NOT NULL,use_proxy BOOLEAN NOT NULL,bot_token TEXT NOT NULL,chat_id VARCHAR(128) NOT NULL,thread_id BIGINT NOT NULL CHECK(thread_id>=0),webhook_url TEXT NOT NULL,bearer_token TEXT NOT NULL,position INTEGER NOT NULL,updated_at VARCHAR(64) NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS media_directories(owner_key VARCHAR(128) PRIMARY KEY,directory_name VARCHAR(128) NOT NULL UNIQUE,created_at VARCHAR(64) NOT NULL)`,
}

func migrateSettings(db *database.DB) error {
	if err := upgradeStorageKinds(db); err != nil {
		return err
	}
	for _, q := range settingsSchema {
		if db.Dialect == "mysql" {
			q += " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"
		}
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	// Column discovery works on both SQLite and MySQL, including installations
	// created before request pacing was configurable.
	rows, err := db.Query("SELECT * FROM system_settings WHERE 1=0")
	if err != nil {
		return err
	}
	columns, err := rows.Columns()
	rows.Close()
	if err != nil {
		return err
	}
	has := make(map[string]bool, len(columns))
	for _, column := range columns {
		has[column] = true
	}
	hadScrollJitter := has["scroll_jitter_seconds"]
	for _, migration := range []struct{ name, definition string }{
		{"browser_mode", "VARCHAR(16) NOT NULL DEFAULT 'local' CHECK(browser_mode IN ('local','cdp'))"},
		{"cdp_host", "VARCHAR(255) NOT NULL DEFAULT '127.0.0.1'"},
		{"cdp_port", "INTEGER NOT NULL DEFAULT 9222 CHECK(cdp_port BETWEEN 1 AND 65535)"},
		{"scroll_jitter_seconds", "INTEGER NOT NULL DEFAULT 0 CHECK(scroll_jitter_seconds>=0)"},
		{"queue_interval_seconds", "INTEGER NOT NULL DEFAULT 0 CHECK(queue_interval_seconds BETWEEN 0 AND 3600)"},
		{"request_interval_seconds", "INTEGER NOT NULL DEFAULT 5 CHECK(request_interval_seconds>=5)"},
		{"request_jitter_seconds", "INTEGER NOT NULL DEFAULT 0 CHECK(request_jitter_seconds>=0)"},
		{"max_posts_per_run", "INTEGER NOT NULL DEFAULT 1000 CHECK(max_posts_per_run BETWEEN 1 AND 10000)"},
		{"page_budget", "INTEGER NOT NULL DEFAULT 15 CHECK(page_budget BETWEEN 1 AND 100)"},
		{"timeline_idle_seconds", "INTEGER NOT NULL DEFAULT 24 CHECK(timeline_idle_seconds BETWEEN 1 AND 300)"},
		{"known_boundary_posts", "INTEGER NOT NULL DEFAULT 8 CHECK(known_boundary_posts BETWEEN 1 AND 100)"},
		{"request_budget_mode", "VARCHAR(16) NOT NULL DEFAULT 'balanced' CHECK(request_budget_mode IN ('conservative','balanced','fast','turbo'))"},
		{"rate_limit_mode", "VARCHAR(16) NOT NULL DEFAULT 'balanced' CHECK(rate_limit_mode IN ('conservative','balanced','fast'))"},
		{"browser_max_age_seconds", "INTEGER NOT NULL DEFAULT 21600 CHECK(browser_max_age_seconds BETWEEN 300 AND 86400)"},
		{"browser_max_tasks", "INTEGER NOT NULL DEFAULT 100 CHECK(browser_max_tasks BETWEEN 1 AND 1000)"},
		{"media_retry_initial_seconds", "INTEGER NOT NULL DEFAULT 300 CHECK(media_retry_initial_seconds BETWEEN 60 AND 86400)"},
		{"media_retry_max_seconds", "INTEGER NOT NULL DEFAULT 86400 CHECK(media_retry_max_seconds BETWEEN 60 AND 604800)"},
		{"archive_theme", "VARCHAR(16) NOT NULL DEFAULT 'system' CHECK(archive_theme IN ('system','light','dark'))"},
		{"timeline_page_size", "INTEGER NOT NULL DEFAULT 24 CHECK(timeline_page_size BETWEEN 10 AND 1000)"},
		{"video_default_muted", "BOOLEAN NOT NULL DEFAULT 1"},
		{"sqlite_read_connections", "INTEGER NOT NULL DEFAULT 4 CHECK(sqlite_read_connections BETWEEN 1 AND 16)"},
		{"video_loop", "BOOLEAN NOT NULL DEFAULT 0"},
		{"video_auto_next", "BOOLEAN NOT NULL DEFAULT 0"},
		{"video_timeline_autoplay", "BOOLEAN NOT NULL DEFAULT 0"},
		{"video_timeline_preload", "BOOLEAN NOT NULL DEFAULT 0"},
		{"media_preload_mode", "VARCHAR(16) NOT NULL DEFAULT 'standard'"},
		{"image_cache_seconds", "INTEGER NOT NULL DEFAULT 604800 CHECK(image_cache_seconds BETWEEN 3600 AND 2592000)"},
		{"video_cache_seconds", "INTEGER NOT NULL DEFAULT 600 CHECK(video_cache_seconds BETWEEN 60 AND 86400)"},
	} {
		if !has[migration.name] {
			if _, err = db.Exec("ALTER TABLE system_settings ADD COLUMN " + migration.name + " " + migration.definition); err != nil {
				return err
			}
		}
	}
	if !hadScrollJitter {
		// Give upgraded installations natural variation without making a
		// one-second legacy scroll delay invalid.
		if _, err = db.Exec("UPDATE system_settings SET scroll_jitter_seconds=CASE WHEN scroll_delay_seconds>1 THEN 1 ELSE 0 END"); err != nil {
			return err
		}
	}
	if _, err = db.Exec("UPDATE system_settings SET timeline_page_size=10 WHERE timeline_page_size<10"); err != nil {
		return err
	}
	if err = upgradeRequestBudgetModeConstraint(db); err != nil {
		return err
	}
	if _, err = db.Exec("UPDATE system_settings SET default_interval_seconds=3600 WHERE default_interval_seconds<3600"); err != nil {
		return err
	}
	_, err = db.Exec(database.Upsert(db.Dialect, "INSERT INTO schema_meta(id,version,updated_at) VALUES(1,?,?)", []string{"id"}, []string{"version", "updated_at"}), 22, now())
	return err
}

func upgradeRequestBudgetModeConstraint(db *database.DB) error {
	if db.Dialect == "mysql" {
		return upgradeMySQLRequestBudgetModeConstraint(db)
	}
	var schema string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='system_settings'").Scan(&schema); err != nil {
		return err
	}
	if strings.Contains(schema, "'turbo'") && strings.Contains(schema, "timeline_page_size BETWEEN 10 AND 1000") {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const temporary = "system_settings_budget_upgrade"
	if _, err = tx.Exec("DROP TABLE IF EXISTS " + temporary); err != nil {
		return err
	}
	create := strings.Replace(systemSettingsDDL, "CREATE TABLE IF NOT EXISTS system_settings", "CREATE TABLE "+temporary, 1)
	if _, err = tx.Exec(create); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO " + temporary + "(" + systemSettingsColumns + ") SELECT " + systemSettingsColumns + " FROM system_settings"); err != nil {
		return err
	}
	if _, err = tx.Exec("DROP TABLE system_settings"); err != nil {
		return err
	}
	if _, err = tx.Exec("ALTER TABLE " + temporary + " RENAME TO system_settings"); err != nil {
		return err
	}
	return tx.Commit()
}

func upgradeMySQLRequestBudgetModeConstraint(db *database.DB) error {
	rows, err := db.CheckConstraints("system_settings")
	if err != nil {
		return err
	}
	var old []string
	needBudget, needPageSize := false, false
	for rows.Next() {
		var name, clause string
		if err = rows.Scan(&name, &clause); err != nil {
			rows.Close()
			return err
		}
		clause = strings.ToLower(clause)
		if strings.Contains(clause, "request_budget_mode") && !strings.Contains(clause, "'turbo'") {
			old = append(old, name)
			needBudget = true
		} else if strings.Contains(clause, "timeline_page_size") && !strings.Contains(clause, "between 10 and 1000") {
			old = append(old, name)
			needPageSize = true
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	alter := make([]string, 0, len(old)+1)
	for _, name := range old {
		drop := db.DropCheckConstraint(name)
		// MariaDB's inline column checks must be removed by redefining the
		// column; separately named table checks use DROP CONSTRAINT.
		if db.MariaDB && name == "request_budget_mode" {
			drop = "MODIFY COLUMN request_budget_mode VARCHAR(16) NOT NULL DEFAULT 'balanced'"
		} else if db.MariaDB && name == "timeline_page_size" {
			drop = "MODIFY COLUMN timeline_page_size INTEGER NOT NULL DEFAULT 24"
		}
		alter = append(alter, drop)
	}
	if needBudget {
		alter = append(alter, "ADD CONSTRAINT system_settings_request_budget_mode CHECK(request_budget_mode IN ('conservative','balanced','fast','turbo'))")
	}
	if needPageSize {
		alter = append(alter, "ADD CONSTRAINT system_settings_timeline_page_size CHECK(timeline_page_size BETWEEN 10 AND 1000)")
	}
	if len(alter) == 0 {
		return nil
	}
	_, err = db.Exec("ALTER TABLE system_settings " + strings.Join(alter, ","))
	return err
}

func ensureRunJobColumn(db *database.DB) error {
	rows, err := db.Query("SELECT * FROM runs WHERE 1=0")
	if err != nil {
		return err
	}
	columns, err := rows.Columns()
	rows.Close()
	if err != nil {
		return err
	}
	for _, column := range columns {
		if column == "job_id" {
			return nil
		}
	}
	_, err = db.Exec("ALTER TABLE runs ADD COLUMN job_id BIGINT NOT NULL DEFAULT 0")
	return err
}
func ensureArchiveIndexes(db *database.DB) error {
	if err := ensureReplyMetadata(db); err != nil {
		return err
	}
	for _, idx := range []struct{ name, table, columns string }{
		{"posts_author", "posts", "author_id"},
		{"posts_quote", "posts", "quote_id"},
		{"posts_reply", "posts", "reply_to"},
		{"api_idempotency_expiry", "api_idempotency", "expires_at"},
		{"api_idempotency_credential", "api_idempotency", "credential_id"},
		{"api_audit_created", "api_audit", "created_at,id"},
		{"target_identity_user", "target_identities", "user_id"},
		{"target_identity_handle", "target_identities", "current_username"},
		{"media_storage", "media", "storage_id"},
		{"runs_target", "runs", "target,id"},
		{"runs_job", "runs", "job_id"},
		{"target_posts_post", "target_posts", "post_id"},
		// Snowflake IDs are strings; include their length to preserve numeric
		// chronology, including older IDs with fewer digits.
		{"posts_timeline", "posts", "(length(id)),id"},
		{"target_posts_timeline", "target_posts", "target,(length(post_id)),post_id"},
		{"posts_first_seen", "posts", "first_seen"},
		{"media_status_bytes", "media", "status,bytes"},
		{"media_post_status", "media", "post_id,status"},
	} {
		if db.MariaDB && (idx.name == "posts_timeline" || idx.name == "target_posts_timeline") {
			column := "id"
			idx.columns = "xm_id_length,id"
			if idx.table == "target_posts" {
				column = "post_id"
				idx.columns = "target,xm_id_length,post_id"
			}
			// Invisible generated columns are omitted from SELECT * and implicit
			// INSERTs, preserving the business schema used by bidirectional copies.
			var exists int
			if err := db.QueryRow("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name='xm_id_length'", idx.table).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				if _, err := db.Exec("ALTER TABLE " + idx.table + " ADD COLUMN xm_id_length SMALLINT UNSIGNED GENERATED ALWAYS AS (OCTET_LENGTH(" + column + ")) VIRTUAL INVISIBLE"); err != nil {
					return err
				}
			}
		}
		var n int
		var err error
		if db.Dialect == "mysql" {
			err = db.QueryRow("SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=?", idx.table, idx.name).Scan(&n)
		} else {
			err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", idx.name).Scan(&n)
		}
		if err != nil {
			return err
		}
		if n == 0 {
			if _, err = db.Exec("CREATE INDEX " + idx.name + " ON " + idx.table + "(" + idx.columns + ")"); err != nil {
				return err
			}
		}
	}
	return nil
}
