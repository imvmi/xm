package web

import "xm/internal/database"

// MigrateStore prepares scheduler/notification tables without starting the UI.
func MigrateStore(db *database.DB) error {
	for _, query := range mysqlWebSchema {
		if _, err := db.Exec(query); err != nil {
			return err
		}
	}
	if err := ensureRunJobColumn(db); err != nil {
		return err
	}
	return ensureCollectionModeSchema(db)
}

var mysqlWebSchema = []string{
	`CREATE TABLE IF NOT EXISTS monitors(username VARCHAR(64) PRIMARY KEY,replies BOOLEAN NOT NULL DEFAULT 0,enabled BOOLEAN NOT NULL DEFAULT 1,requested BOOLEAN NOT NULL DEFAULT 0,running BOOLEAN NOT NULL DEFAULT 0,next_run BIGINT NOT NULL,last_started BIGINT NOT NULL DEFAULT 0,last_finished BIGINT NOT NULL DEFAULT 0,last_success BIGINT NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT (''),failures INT NOT NULL DEFAULT 0,last_posts INT NOT NULL DEFAULT 0,new_posts INT NOT NULL DEFAULT 0,catchup BOOLEAN NOT NULL DEFAULT 0,catchup_post_anchor VARCHAR(64) NOT NULL DEFAULT '',catchup_reply_anchor VARCHAR(64) NOT NULL DEFAULT '',catchup_scroll_budget INT NOT NULL DEFAULT 0,baseline_status VARCHAR(32) NOT NULL DEFAULT 'not_started',posts_baseline_status VARCHAR(32) NOT NULL DEFAULT 'not_started',replies_baseline_status VARCHAR(32) NOT NULL DEFAULT 'not_enabled',baseline_started BIGINT NOT NULL DEFAULT 0,baseline_finished BIGINT NOT NULL DEFAULT 0,baseline_stop_reason TEXT NOT NULL DEFAULT (''),INDEX monitors_due(next_run)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS collection_jobs(id BIGINT PRIMARY KEY AUTO_INCREMENT,target VARCHAR(64) NOT NULL,source VARCHAR(16) NOT NULL,status VARCHAR(32) NOT NULL,queued_at BIGINT NOT NULL,queue_order BIGINT NOT NULL,started_at BIGINT NOT NULL DEFAULT 0,finished_at BIGINT NOT NULL DEFAULT 0,run_id BIGINT NOT NULL DEFAULT 0,attempts INT NOT NULL DEFAULT 0,retry_at BIGINT NOT NULL DEFAULT 0,retry_count INT NOT NULL DEFAULT 0,message TEXT NOT NULL DEFAULT (''),mode VARCHAR(16) NOT NULL DEFAULT 'incremental',INDEX collection_jobs_queue(finished_at,status,queue_order,id),INDEX collection_jobs_target(target,finished_at)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	"CREATE TABLE IF NOT EXISTS web_state (`key` VARCHAR(128) PRIMARY KEY,value TEXT NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin",
	`CREATE TABLE IF NOT EXISTS notification_targets(target VARCHAR(64) PRIMARY KEY,ready BOOLEAN NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS notification_seen(target VARCHAR(64) NOT NULL,post_id VARCHAR(64) NOT NULL,PRIMARY KEY(target,post_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	"CREATE TABLE IF NOT EXISTS notification_incidents(`key` VARCHAR(128) PRIMARY KEY,kind VARCHAR(32) NOT NULL,last_notice BIGINT NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin",
	`CREATE TABLE IF NOT EXISTS notification_deliveries(id BIGINT PRIMARY KEY AUTO_INCREMENT,event_id VARCHAR(255) NOT NULL,channel_id VARCHAR(64) NOT NULL,destination VARCHAR(64) NOT NULL,payload TEXT NOT NULL,status VARCHAR(32) NOT NULL DEFAULT 'pending',attempts INT NOT NULL DEFAULT 0,next_attempt BIGINT NOT NULL,created_at BIGINT NOT NULL,finished_at BIGINT NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT (''),UNIQUE KEY event_channel(event_id,channel_id),INDEX notification_due(status,next_attempt)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
}
