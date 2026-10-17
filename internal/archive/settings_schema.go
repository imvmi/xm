package archive

import "xm/internal/database"

// Shared logical schema: SQLite and MySQL use the same keys and constraints.
// Per-service credentials are columns, not one opaque configuration document.
var settingsSchema = []string{
	`CREATE TABLE IF NOT EXISTS schema_meta(id INTEGER PRIMARY KEY CHECK(id=1),version INTEGER NOT NULL,updated_at VARCHAR(64) NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS admin_accounts(id INTEGER PRIMARY KEY CHECK(id=1),username VARCHAR(32) NOT NULL UNIQUE,password_salt BLOB NOT NULL CHECK(length(password_salt)=32),password_hash BLOB NOT NULL CHECK(length(password_hash)=32),configuration_locked BOOLEAN NOT NULL CHECK(configuration_locked=1),created_at VARCHAR(64) NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS storage_profiles(id VARCHAR(64) PRIMARY KEY,name VARCHAR(100) NOT NULL,kind VARCHAR(16) NOT NULL CHECK(kind IN ('local','webdav','s3')),endpoint TEXT NOT NULL,object_prefix TEXT NOT NULL,username TEXT NOT NULL,password TEXT NOT NULL,region TEXT NOT NULL,bucket TEXT NOT NULL,access_key TEXT NOT NULL,secret_key TEXT NOT NULL,session_token TEXT NOT NULL,path_style BOOLEAN NOT NULL,use_proxy BOOLEAN NOT NULL,timeout_seconds INTEGER NOT NULL CHECK(timeout_seconds BETWEEN 5 AND 3600),position INTEGER NOT NULL,updated_at VARCHAR(64) NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS system_settings(id INTEGER PRIMARY KEY CHECK(id=1),proxy TEXT NOT NULL,chrome_path TEXT NOT NULL,headless BOOLEAN NOT NULL,request_timeout_seconds INTEGER NOT NULL CHECK(request_timeout_seconds>0),collection_timeout_seconds INTEGER NOT NULL CHECK(collection_timeout_seconds>0),scroll_delay_seconds INTEGER NOT NULL CHECK(scroll_delay_seconds>0),max_scrolls INTEGER NOT NULL CHECK(max_scrolls>0),max_media_bytes BIGINT NOT NULL CHECK(max_media_bytes>0),default_interval_seconds INTEGER NOT NULL CHECK(default_interval_seconds BETWEEN 60 AND 604800),polling_paused BOOLEAN NOT NULL,active_storage_id VARCHAR(64) NOT NULL,updated_at VARCHAR(64) NOT NULL,FOREIGN KEY(active_storage_id) REFERENCES storage_profiles(id))`,
	`CREATE TABLE IF NOT EXISTS notification_channels(id VARCHAR(64) PRIMARY KEY,name VARCHAR(100) NOT NULL,kind VARCHAR(16) NOT NULL CHECK(kind IN ('telegram','webhook')),enabled BOOLEAN NOT NULL,updates BOOLEAN NOT NULL,alerts BOOLEAN NOT NULL,use_proxy BOOLEAN NOT NULL,bot_token TEXT NOT NULL,chat_id VARCHAR(128) NOT NULL,thread_id BIGINT NOT NULL CHECK(thread_id>=0),webhook_url TEXT NOT NULL,bearer_token TEXT NOT NULL,position INTEGER NOT NULL,updated_at VARCHAR(64) NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS media_directories(owner_key VARCHAR(128) PRIMARY KEY,directory_name VARCHAR(128) NOT NULL UNIQUE,created_at VARCHAR(64) NOT NULL)`,
}

func migrateSettings(db *database.DB) error {
	for _, q := range settingsSchema {
		if db.Dialect == "mysql" {
			q += " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"
		}
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	_, err := db.Exec(database.Upsert(db.Dialect, "INSERT INTO schema_meta(id,version,updated_at) VALUES(1,?,?)", []string{"id"}, []string{"version", "updated_at"}), 2, now())
	return err
}
func ensureArchiveIndexes(db *database.DB) error {
	for _, idx := range []struct{ name, table, columns string }{
		{"posts_author", "posts", "author_id"},
		{"target_identity_user", "target_identities", "user_id"},
		{"target_identity_handle", "target_identities", "current_username"},
		{"media_storage", "media", "storage_id"},
		{"runs_target", "runs", "target,id"},
		{"target_posts_post", "target_posts", "post_id"},
	} {
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
