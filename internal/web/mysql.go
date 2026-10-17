package web

import "xm/internal/database"

// MigrateStore prepares scheduler/notification tables without starting the UI.
func MigrateStore(db *database.DB) error {
	for _, query := range mysqlWebSchema {
		if _, err := db.Exec(query); err != nil {
			return err
		}
	}
	return nil
}

var mysqlWebSchema = []string{
	`CREATE TABLE IF NOT EXISTS monitors(username VARCHAR(64) PRIMARY KEY,interval_seconds INT NOT NULL,post_limit INT NOT NULL,replies BOOLEAN NOT NULL DEFAULT 0,enabled BOOLEAN NOT NULL DEFAULT 1,requested BOOLEAN NOT NULL DEFAULT 0,running BOOLEAN NOT NULL DEFAULT 0,next_run BIGINT NOT NULL,last_started BIGINT NOT NULL DEFAULT 0,last_finished BIGINT NOT NULL DEFAULT 0,last_success BIGINT NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT (''),failures INT NOT NULL DEFAULT 0,last_posts INT NOT NULL DEFAULT 0,new_posts INT NOT NULL DEFAULT 0,INDEX monitors_due(next_run)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	"CREATE TABLE IF NOT EXISTS web_state (`key` VARCHAR(128) PRIMARY KEY,value TEXT NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin",
	`CREATE TABLE IF NOT EXISTS notification_targets(target VARCHAR(64) PRIMARY KEY,ready BOOLEAN NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS notification_seen(target VARCHAR(64) NOT NULL,post_id VARCHAR(64) NOT NULL,PRIMARY KEY(target,post_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	"CREATE TABLE IF NOT EXISTS notification_incidents(`key` VARCHAR(128) PRIMARY KEY,kind VARCHAR(32) NOT NULL,last_notice BIGINT NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin",
	`CREATE TABLE IF NOT EXISTS notification_deliveries(id BIGINT PRIMARY KEY AUTO_INCREMENT,event_id VARCHAR(255) NOT NULL,channel_id VARCHAR(64) NOT NULL,destination VARCHAR(64) NOT NULL,payload TEXT NOT NULL,status VARCHAR(32) NOT NULL DEFAULT 'pending',attempts INT NOT NULL DEFAULT 0,next_attempt BIGINT NOT NULL,created_at BIGINT NOT NULL,finished_at BIGINT NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT (''),UNIQUE KEY event_channel(event_id,channel_id),INDEX notification_due(status,next_attempt)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
}
