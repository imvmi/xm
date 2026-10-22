package archive

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

var archiveTables = []string{"avatar_sources", "api_credentials", "api_idempotency", "api_audit", "storage_profiles", "notification_channels", "system_settings", "admin_accounts", "media_directories", "cookie_sources", "x_cookies", "posts", "post_bookmarks", "profiles", "target_identities", "account_preferences", "target_posts", "media", "media_local_copies", "runs", "request_budgets", "run_metrics", "collection_progress", "monitors", "collection_jobs", "web_state", "notification_targets", "notification_seen", "notification_incidents", "notification_deliveries"}

// CopySQLiteToMySQL copies a consistent read-only SQLite snapshot into an empty
// MySQL schema. Destination writes commit as one transaction; source is untouched.
// Caller must prepare all tables and hold both source and destination worker locks.
func (s *Store) CopySQLiteToMySQL(ctx context.Context, source string) (map[string]int64, error) {
	if s.DB.Dialect != "mysql" {
		return nil, errors.New("迁移目标必须配置为 mysql")
	}
	full, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(full)
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("SQLite 源文件不存在或不是普通文件")
	}
	u := url.URL{Scheme: "file", Path: full}
	src, err := sql.Open("sqlite", u.String()+"?mode=ro")
	if err != nil {
		return nil, errors.New("无法打开 SQLite 源文件")
	}
	defer src.Close()
	src.SetMaxOpenConns(1)
	read, err := src.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, errors.New("无法读取 SQLite 源文件")
	}
	defer read.Rollback()
	write, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, errors.New("无法开始 MySQL 迁移事务")
	}
	defer write.Rollback()
	for _, table := range archiveTables {
		var n int64
		if err = write.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&n); err != nil {
			return nil, fmt.Errorf("无法检查目标表 %s", table)
		}
		if n != 0 {
			return nil, fmt.Errorf("目标表 %s 非空，拒绝覆盖；请使用新的空数据库", table)
		}
	}
	result := map[string]int64{}
	for _, table := range archiveTables {
		var exists int
		if err = read.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&exists); err != nil {
			return nil, errors.New("无法读取 SQLite 表信息")
		}
		if exists == 0 {
			switch table {
			case "avatar_sources", "api_credentials", "api_idempotency", "api_audit", "media_local_copies", "account_preferences", "post_bookmarks", "request_budgets", "run_metrics", "collection_progress", "cookie_sources", "monitors", "collection_jobs", "web_state", "notification_targets", "notification_seen", "notification_incidents", "notification_deliveries":
				// CLI-only archives may not have started the web service yet.
			default:
				return nil, fmt.Errorf("源数据库缺少当前版本必需的表 %s，不支持旧版结构迁移", table)
			}
			continue
		}
		rows, err := read.QueryContext(ctx, "SELECT * FROM `"+table+"`")
		if err != nil {
			return nil, fmt.Errorf("读取源表 %s 失败", table)
		}
		cols, err := rows.Columns()
		if err != nil {
			rows.Close()
			return nil, err
		}
		targetRows, err := write.QueryContext(ctx, "SELECT * FROM `"+table+"` WHERE 1=0")
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("读取目标表 %s 结构失败", table)
		}
		targetCols, err := targetRows.Columns()
		targetRows.Close()
		if err != nil {
			rows.Close()
			return nil, err
		}
		allowed := map[string]bool{}
		for _, c := range targetCols {
			allowed[c] = true
		}
		for _, c := range cols {
			if !allowed[c] {
				rows.Close()
				return nil, fmt.Errorf("源表 %s 包含当前版本不支持的字段", table)
			}
		}
		query := "INSERT INTO `" + table + "` (`" + strings.Join(cols, "`,`") + "`) VALUES (" + strings.TrimRight(strings.Repeat("?,", len(cols)), ",") + ")"
		statement, err := write.PrepareContext(ctx, query)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("准备迁移表 %s 失败", table)
		}
		var count int64
		for rows.Next() {
			values := make([]any, len(cols))
			pointers := make([]any, len(cols))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err = rows.Scan(pointers...); err != nil {
				break
			}
			if _, err = statement.ExecContext(ctx, values...); err != nil {
				break
			}
			count++
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		statement.Close()
		if err != nil {
			return nil, fmt.Errorf("迁移表 %s 失败，所有目标写入已回滚；请检查字段长度、数据库权限和连接", table)
		}
		result[table] = count
	}
	if err = write.Commit(); err != nil {
		return nil, errors.New("MySQL 迁移提交未确认，请检查目标数据库，不要直接重复运行")
	}
	return result, nil
}
