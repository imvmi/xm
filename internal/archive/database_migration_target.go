package archive

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/go-sql-driver/mysql"
	"xm/internal/database"
)

// OpenMigrationMySQL connects without creating or changing any tables.
func OpenMigrationMySQL(ctx context.Context, cfg DatabaseConfig) (*Store, error) {
	if cfg.DriverName() != "mysql" {
		return nil, errors.New("请选择 MySQL 目标数据库")
	}
	mc, err := mysqlConfig(cfg)
	if err != nil {
		return nil, err
	}
	c, err := mysql.NewConnector(mc)
	if err != nil {
		return nil, errors.New("MySQL 连接配置无效")
	}
	db := sql.OpenDB(c)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, mysqlError("连接", err)
	}
	return &Store{DB: &database.DB{DB: db, Dialect: "mysql"}}, nil
}

// CheckMigrationEmpty also rejects unrelated tables and nontransactional tables.
// Empty XM tables left by an interrupted attempt are safe to reuse.
func (s *Store) CheckMigrationEmpty(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, "SELECT table_name, COALESCE(engine,'') FROM information_schema.tables WHERE table_schema=DATABASE()")
	if err != nil {
		return errors.New("无法检查 MySQL 目标表")
	}
	var tables []string
	allowed := map[string]bool{"schema_meta": true}
	for _, name := range archiveTables {
		allowed[name] = true
	}
	for rows.Next() {
		var name, engine string
		if err = rows.Scan(&name, &engine); err != nil {
			break
		}
		if !allowed[name] || !strings.EqualFold(engine, "InnoDB") {
			rows.Close()
			return errors.New("目标包含非 XM 表或非 InnoDB 表，请使用独立的空 MySQL 数据库")
		}
		if name != "schema_meta" {
			tables = append(tables, name)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return errors.New("无法读取 MySQL 目标表信息")
	}
	for _, table := range tables {
		var count int64
		if err = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&count); err != nil {
			return errors.New("无法读取 MySQL 目标表")
		}
		if count != 0 {
			return errors.New("目标数据库非空，拒绝覆盖；请使用新的空数据库")
		}
	}
	return nil
}
