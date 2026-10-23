package archive

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"xm/internal/database"
)

type DatabaseConfig struct {
	Driver      string `json:"driver,omitempty" yaml:"driver,omitempty"`
	SQLiteFile  string `json:"sqlite_file,omitempty" yaml:"sqlite_file,omitempty"`
	Host        string `json:"host,omitempty" yaml:"host,omitempty"`
	Port        int    `json:"port,omitempty" yaml:"port,omitempty"`
	Name        string `json:"name,omitempty" yaml:"name,omitempty"`
	User        string `json:"user,omitempty" yaml:"user,omitempty"`
	Password    string `json:"password,omitempty" yaml:"password,omitempty"`
	PasswordEnv string `json:"password_env,omitempty" yaml:"password_env,omitempty"`
	TLSMode     string `json:"tls_mode,omitempty" yaml:"tls_mode,omitempty"`
	CAFile      string `json:"ca_file,omitempty" yaml:"ca_file,omitempty"`
	ServerName  string `json:"server_name,omitempty" yaml:"server_name,omitempty"`
}

const DefaultSQLiteFile = "xm.db"

// SQLiteFilename keeps explicit filenames and older implicit archives usable.
// Fresh installations use xm.db; upgrading never silently opens an empty DB
// instead of a pre-existing archive.sqlite.
func (c DatabaseConfig) SQLiteFilename(dir string) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	if c.SQLiteFile != "" {
		return c.SQLiteFile, nil
	}
	for _, name := range []string{DefaultSQLiteFile, "archive.sqlite"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return name, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	return DefaultSQLiteFile, nil
}

func (c DatabaseConfig) DriverName() string {
	if c.Driver == "" {
		return "sqlite"
	}
	return c.Driver
}

// IsMySQL reports whether the configured backend uses the MySQL wire protocol
// and SQL dialect. Keep DriverName unchanged so MariaDB survives config reloads.
func (c DatabaseConfig) IsMySQL() bool {
	return c.DriverName() == "mysql" || c.DriverName() == "mariadb"
}

func (c DatabaseConfig) DisplayName() string {
	switch c.DriverName() {
	case "mysql":
		return "MySQL"
	case "mariadb":
		return "MariaDB"
	default:
		return "SQLite3"
	}
}

func (c DatabaseConfig) Validate() error {
	if c.DriverName() == "sqlite" {
		if c.SQLiteFile != "" && (filepath.Base(c.SQLiteFile) != c.SQLiteFile || strings.ContainsAny(c.SQLiteFile, "\\\x00\r\n") || (!strings.HasSuffix(c.SQLiteFile, ".sqlite") && !strings.HasSuffix(c.SQLiteFile, ".db"))) {
			return errors.New("SQLite 文件名必须是数据目录中的 .db 或 .sqlite 文件")
		}
		return nil
	}
	if !c.IsMySQL() {
		return errors.New("database.driver 仅支持 sqlite、mysql 或 mariadb")
	}
	if c.Host == "" || strings.ContainsAny(c.Host, "/\\\x00\r\n") || c.Name == "" || len(c.Name) > 64 || c.User == "" || c.Port < 0 || c.Port > 65535 {
		return errors.New("请填写有效的 MySQL / MariaDB host、port、name 和 user")
	}
	if c.TLSMode != "" && c.TLSMode != "verify_identity" && c.TLSMode != "disabled" {
		return errors.New("MySQL / MariaDB TLS 模式仅支持 verify_identity（默认）或 disabled（不加密）")
	}
	return nil
}
func (c *DatabaseConfig) resolvePaths(base string) error {
	if c.CAFile != "" && !filepath.IsAbs(c.CAFile) {
		path, err := filepath.Abs(filepath.Join(base, c.CAFile))
		if err != nil {
			return err
		}
		c.CAFile = path
	}
	return nil
}
func mysqlConfig(c DatabaseConfig) (*mysql.Config, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	mc := mysql.NewConfig()
	mc.Net = "tcp"
	port := c.Port
	if port == 0 {
		port = 3306
	}
	mc.Addr = net.JoinHostPort(strings.Trim(c.Host, "[]"), strconv.Itoa(port))
	mc.User = c.User
	mc.Passwd = c.Password
	mc.DBName = c.Name
	if c.PasswordEnv != "" {
		value, ok := os.LookupEnv(c.PasswordEnv)
		if !ok || value == "" {
			return nil, errors.New("MySQL / MariaDB 密码环境变量未设置或为空")
		}
		mc.Passwd = value
	}
	mc.Timeout = 10 * time.Second
	mc.ReadTimeout = 60 * time.Second
	mc.WriteTimeout = 60 * time.Second
	if err := mc.Apply(mysql.Charset("utf8mb4", "utf8mb4_bin")); err != nil {
		return nil, errors.New("MySQL / MariaDB 字符集配置失败")
	}
	mc.Params = map[string]string{"sql_mode": "'STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION,ONLY_FULL_GROUP_BY'"}
	mc.Logger = log.New(io.Discard, "", 0) // Driver diagnostics may include endpoint/user details.
	if c.TLSMode != "disabled" {
		name := c.ServerName
		if name == "" {
			name = strings.Trim(c.Host, "[]")
		}
		tc := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: name}
		if c.CAFile != "" {
			pem, err := os.ReadFile(c.CAFile)
			if err != nil {
				return nil, errors.New("无法读取 MySQL / MariaDB CA 证书文件")
			}
			roots, err := x509.SystemCertPool()
			if err != nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(pem) {
				return nil, errors.New("MySQL / MariaDB CA 证书文件不包含有效 PEM 证书")
			}
			tc.RootCAs = roots
		}
		mc.TLS = tc
	}
	return mc, nil
}
func OpenConfiguredStore(cfg Config) (*Store, error) {
	if cfg.Database.DriverName() == "sqlite" {
		name, err := cfg.Database.SQLiteFilename(cfg.DataDir)
		if err != nil {
			return nil, err
		}
		return OpenStoreFile(cfg.DataDir, name)
	}
	if err := PrivateDir(cfg.DataDir); err != nil {
		return nil, err
	}
	mc, err := mysqlConfig(cfg.Database)
	if err != nil {
		return nil, err
	}
	connector, err := mysql.NewConnector(mc)
	if err != nil {
		return nil, errors.New("MySQL / MariaDB 连接配置无效")
	}
	raw := sql.OpenDB(connector)
	raw.SetMaxOpenConns(8)
	raw.SetMaxIdleConns(4)
	raw.SetConnMaxLifetime(3 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err = raw.PingContext(ctx); err != nil {
		raw.Close()
		return nil, mysqlError("连接", err)
	}
	d := &database.DB{DB: raw, Dialect: "mysql"}
	var version string
	if err = raw.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); err != nil {
		raw.Close()
		return nil, mysqlError("读取服务器版本", err)
	}
	d.MariaDB = strings.Contains(strings.ToLower(version), "mariadb")
	for _, query := range mysqlArchiveSchema {
		if _, err = d.ExecContext(ctx, query); err != nil {
			raw.Close()
			return nil, mysqlError("初始化表结构", err)
		}
	}
	if err = migrateSettings(d); err != nil {
		raw.Close()
		return nil, mysqlError("初始化配置表", err)
	}
	if err = ensureRunJobColumn(d); err != nil {
		raw.Close()
		return nil, mysqlError("升级采集记录", err)
	}
	if err = ensureArchiveIndexes(d); err != nil {
		raw.Close()
		return nil, mysqlError("初始化索引", err)
	}
	return &Store{DB: d}, nil
}
func mysqlError(action string, err error) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return fmt.Errorf("MySQL / MariaDB %s失败（错误码 %d），请检查数据库名称、权限和配置", action, me.Number)
	}
	return fmt.Errorf("MySQL / MariaDB %s失败，请检查网络、TLS 证书和连接配置", action)
}

// InnoDB, binary UTF-8 comparison and bounded indexed identifiers preserve the
// archive's string IDs and allow full Unicode text including emoji.
var mysqlArchiveSchema = []string{
	`CREATE TABLE IF NOT EXISTS x_cookies(id INTEGER PRIMARY KEY CHECK(id=1),auth_token TEXT NOT NULL,ct0 TEXT NOT NULL,updated_at VARCHAR(64) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS posts(id VARCHAR(64) PRIMARY KEY,author_id VARCHAR(64) NOT NULL,username VARCHAR(64) NOT NULL,text LONGTEXT NOT NULL,created_at VARCHAR(64) NOT NULL,url TEXT NOT NULL,reply_to VARCHAR(64) NOT NULL,quote_id VARCHAR(64) NOT NULL,repost_id VARCHAR(64) NOT NULL,raw_json LONGTEXT NOT NULL,first_seen VARCHAR(64) NOT NULL,last_seen VARCHAR(64) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS post_bookmarks(post_id VARCHAR(64) PRIMARY KEY,created_at BIGINT NOT NULL CHECK(created_at>0),INDEX post_bookmarks_recent(created_at,post_id),FOREIGN KEY(post_id) REFERENCES posts(id) ON DELETE CASCADE) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS target_posts(target VARCHAR(64) NOT NULL,post_id VARCHAR(64) NOT NULL,PRIMARY KEY(target,post_id),INDEX target_posts_post(post_id),FOREIGN KEY(post_id) REFERENCES posts(id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS target_identities(target VARCHAR(64) PRIMARY KEY,user_id VARCHAR(64) NOT NULL,current_username VARCHAR(64) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS profiles(id VARCHAR(64) PRIMARY KEY,username VARCHAR(64) NOT NULL,protected BOOLEAN NOT NULL,checked_at VARCHAR(64) NOT NULL) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS media(post_id VARCHAR(64) NOT NULL,media_id VARCHAR(128) NOT NULL,kind VARCHAR(32) NOT NULL,source_url TEXT NOT NULL,status VARCHAR(32) NOT NULL DEFAULT 'pending',local_path VARCHAR(2048) NOT NULL DEFAULT '',bytes BIGINT NOT NULL DEFAULT 0,sha256 VARCHAR(64) NOT NULL DEFAULT '',attempts INT NOT NULL DEFAULT 0,next_attempt BIGINT NOT NULL DEFAULT 0,last_error TEXT NOT NULL DEFAULT (''),updated_at VARCHAR(64) NOT NULL,storage_id VARCHAR(64) NOT NULL DEFAULT 'local',object_key VARCHAR(1024) NOT NULL DEFAULT '',PRIMARY KEY(post_id,media_id),INDEX media_status(status,next_attempt),FOREIGN KEY(post_id) REFERENCES posts(id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
	`CREATE TABLE IF NOT EXISTS runs(id BIGINT PRIMARY KEY AUTO_INCREMENT,target VARCHAR(64) NOT NULL,started_at VARCHAR(64) NOT NULL,finished_at VARCHAR(64),status VARCHAR(32) NOT NULL,posts_seen INT NOT NULL DEFAULT 0,message TEXT NOT NULL DEFAULT (''),job_id BIGINT NOT NULL DEFAULT 0,INDEX runs_job(job_id)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
}
