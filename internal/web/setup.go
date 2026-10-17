package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"xm/internal/archive"
)

// Setup runs before opening the configured database, so a fresh installation can
// configure MySQL in the browser without first requiring a working connection.
// The caller holds the data directory's worker lock throughout setup and serving.
func Setup(ctx context.Context, path string, cfg archive.Config, address string, secure bool) (archive.Config, error) {
	if ready, err := installationReady(cfg); err != nil {
		return cfg, err
	} else if ready {
		return cfg, nil
	}
	l, err := net.Listen("tcp", address)
	if err != nil {
		return cfg, err
	}
	defer l.Close()
	wizard := newSetup(path, cfg)
	fmt.Fprintln(os.Stderr, "XM 首次设置 · Local: http://"+l.Addr().String())
	server := &http.Server{Handler: wizard.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20, BaseContext: func(net.Listener) context.Context { return ctx }}
	errs := make(chan error, 1)
	go func() { errs <- server.Serve(l) }()
	var result archive.Config
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case err = <-errs:
	case result = <-wizard.done:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if server.Shutdown(shutdown) != nil {
		_ = server.Close()
	}
	if err != nil {
		return cfg, err
	}
	return result, nil
}

type setupWizard struct {
	path      string
	cfg       archive.Config
	mu        sync.Mutex
	completed bool
	done      chan archive.Config
}

func newSetup(path string, cfg archive.Config) *setupWizard {
	return &setupWizard{path: path, cfg: cfg, done: make(chan archive.Config, 1)}
}

type setupInput struct {
	Username    string                 `json:"username"`
	Password    string                 `json:"password"`
	Database    archive.DatabaseConfig `json:"database"`
	StorageKind string                 `json:"storage_kind"`
	Storage     archive.StorageProfile `json:"storage"`
}

func (s *setupWizard) candidate(v setupInput) (archive.Config, error) {
	c := s.cfg
	c.Database = v.Database
	// Only reuse an existing password for the exact same database destination.
	old := s.cfg.Database
	if c.Database.Password == "" && c.Database.Host == old.Host && c.Database.Port == old.Port && c.Database.Name == old.Name && c.Database.User == old.User {
		c.Database.Password = old.Password
	}
	if c.Database.CAFile != "" && !filepath.IsAbs(c.Database.CAFile) {
		p, err := filepath.Abs(filepath.Join(filepath.Dir(s.path), c.Database.CAFile))
		if err != nil {
			return c, err
		}
		c.Database.CAFile = p
	}
	if err := c.Database.Validate(); err != nil {
		return c, err
	}
	c.StorageProfiles = append([]archive.StorageProfile(nil), c.StorageProfiles...)
	switch v.StorageKind {
	case "", "keep":
	case "local":
		c.MediaStorage = "local"
	case "webdav", "s3":
		p := v.Storage
		p.Kind = v.StorageKind
		p.ID = "setup_" + randomToken()[:20]
		if p.Name == "" {
			p.Name = map[string]string{"webdav": "我的 WebDAV", "s3": "我的 S3"}[p.Kind]
		}
		if p.Timeout == 0 {
			p.Timeout = 120
		}
		if err := p.Validate(); err != nil {
			return c, err
		}
		c.StorageProfiles = append(c.StorageProfiles, p)
		c.MediaStorage = p.ID
	default:
		return c, errors.New("请选择本地、WebDAV 或 S3 存储")
	}
	return c, c.ValidateStorage()
}

// Switching away from an existing local archive is a migration, not setup.
func (s *setupWizard) guardLocalArchive(c archive.Config) error {
	if c.Database.DriverName() == "sqlite" || s.cfg.Database.DriverName() != "sqlite" {
		return nil
	}
	path := filepath.Join(c.DataDir, "archive.sqlite")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return errors.New("无法检查现有本地归档")
	}
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro")
	if err != nil {
		return errors.New("无法检查现有本地归档")
	}
	defer db.Close()
	for _, table := range []string{"posts", "profiles", "monitors", "runs", "notification_deliveries", "x_cookies", "admin_accounts", "system_settings", "storage_profiles", "notification_channels"} {
		var exists, n int
		if err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&exists); err != nil {
			return errors.New("无法检查现有本地归档")
		}
		if exists == 0 {
			continue
		}
		if err = db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			return errors.New("无法检查现有本地归档")
		}
		if n > 0 {
			return errors.New("检测到已有 SQLite 数据，请选择 SQLite 保留归档，或先按文档使用 migrate-mysql 迁移")
		}
	}
	return nil
}
func (s *setupWizard) checkDatabase(ctx context.Context, c archive.Config) error {
	if err := s.guardLocalArchive(c); err != nil {
		return err
	}
	store, err := archive.OpenConfiguredStore(c)
	if err != nil {
		return err
	}
	defer store.Close()
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	release, err := store.AcquireDatabaseWorker(workerCtx, cancel, c.Database.Name)
	if err != nil {
		return err
	}
	defer release()
	if _, ready, e := loadAdmin(store); e != nil {
		return e
	} else if ready {
		return errors.New("目标数据库已初始化，不能覆盖管理员和存储配置")
	}
	if err = migrate(store); err != nil {
		return errors.New("数据库建表失败，请检查权限和版本")
	}
	tx, err := store.DB.BeginTx(workerCtx, nil)
	if err != nil {
		return errors.New("无法开始数据库测试事务")
	}
	defer tx.Rollback()
	key := "setup_test_" + randomToken()
	if _, err = tx.ExecContext(workerCtx, "INSERT INTO web_state (`key`,value) VALUES(?,?)", key, "test"); err != nil {
		return errors.New("数据库写入测试失败，请检查权限")
	}
	var value string
	if err = tx.QueryRowContext(workerCtx, "SELECT value FROM web_state WHERE `key`=?", key).Scan(&value); err != nil || value != "test" {
		return errors.New("数据库读取测试失败")
	}
	return tx.Rollback()
}
func (s *setupWizard) checkStorage(ctx context.Context, c archive.Config) error {
	if c.ActiveStorage() == "local" {
		dir := filepath.Join(c.DataDir, "media")
		if err := archive.PrivateDir(dir); err != nil {
			return errors.New("本地媒体目录无法创建")
		}
		f, err := os.CreateTemp(dir, ".setup-check-*")
		if err != nil {
			return errors.New("本地媒体目录不可写")
		}
		name := f.Name()
		defer os.Remove(name)
		_, err = f.WriteString("XM storage check")
		syncErr := f.Sync()
		closeErr := f.Close()
		if err != nil || syncErr != nil || closeErr != nil {
			return errors.New("本地媒体写入测试失败")
		}
		data, err := os.ReadFile(name)
		if err != nil || string(data) != "XM storage check" {
			return errors.New("本地媒体读取测试失败")
		}
		return os.Remove(name)
	}
	remote, err := archive.NewRemote(c, c.ActiveStorage())
	if err != nil {
		return err
	}
	defer remote.Close()
	return remote.Test(ctx)
}
func (s *setupWizard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/setup/status", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]bool{"needed": true}) })
	mux.HandleFunc("POST /api/setup/defaults", func(w http.ResponseWriter, r *http.Request) {
		db := s.cfg.Database
		hasPassword := db.Password != ""
		db.Password = ""
		respond(w, 200, map[string]any{"database": db, "password_configured": hasPassword, "has_remote_storage": s.cfg.ActiveStorage() != "local"})
	})
	mux.HandleFunc("POST /api/setup/database-test", func(w http.ResponseWriter, r *http.Request) {
		var v setupInput
		if !decode(w, r, &v) {
			return
		}
		v.StorageKind = "keep"
		c, err := s.candidate(v)
		if err == nil {
			err = s.checkDatabase(r.Context(), c)
		}
		if err != nil {
			problem(w, 400, err.Error())
			return
		}
		respond(w, 200, map[string]string{"message": "数据库建表、读写与事务测试通过"})
	})
	mux.HandleFunc("POST /api/setup/storage-test", func(w http.ResponseWriter, r *http.Request) {
		var v setupInput
		if !decode(w, r, &v) {
			return
		}
		c, err := s.candidate(v)
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		if err == nil {
			err = s.checkDatabase(ctx, c)
		}
		if err == nil {
			err = s.checkStorage(ctx, c)
		}
		if err != nil {
			problem(w, 400, err.Error())
			return
		}
		respond(w, 200, map[string]string{"message": "媒体存储读写与清理测试通过"})
	})
	mux.HandleFunc("POST /api/setup/complete", s.complete)
	assets, _ := fs.Sub(static, "static")
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, _ := static.ReadFile("static/setup.html")
		_, _ = w.Write(b)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
		if r.Method != "GET" && r.Method != "HEAD" {
			if !sameOrigin(r) {
				problem(w, 403, "请求来源不匹配")
				return
			}
			if !s.mu.TryLock() {
				problem(w, 409, "正在测试或保存，请稍后重试")
				return
			}
			defer s.mu.Unlock()
			if ready, err := installationReady(s.cfg); s.completed || ready {
				problem(w, 409, "初始化已完成，请刷新页面登录")
				return
			} else if err != nil {
				problem(w, 500, "无法检查数据库初始化状态")
				return
			}

		}
		mux.ServeHTTP(w, r)
	})
}
func (s *setupWizard) complete(w http.ResponseWriter, r *http.Request) {
	var v setupInput
	if !decode(w, r, &v) {
		return
	}
	record, err := makePasswordRecord(v.Username, v.Password)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	c, err := s.candidate(v)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	if err = s.checkDatabase(ctx, c); err != nil {
		problem(w, 400, err.Error())
		return
	}
	if err = s.checkStorage(ctx, c); err != nil {
		problem(w, 400, "存储测试未通过："+err.Error())
		return
	}
	if err = ctx.Err(); err != nil {
		problem(w, 408, "设置超时，请重试")
		return
	}
	// Hold the database worker lock across the final claim. The bootstrap points
	// at the destination first; admin + runtime then commit together as the marker.
	store, err := archive.OpenConfiguredStore(c)
	if err != nil {
		problem(w, 500, "无法连接初始化数据库")
		return
	}
	defer store.Close()
	workerCtx, stop := context.WithCancel(ctx)
	defer stop()
	release, err := store.AcquireDatabaseWorker(workerCtx, stop, c.Database.Name)
	if err != nil {
		problem(w, 409, "数据库正在使用，请稍后重试")
		return
	}
	defer release()
	if _, ready, e := loadAdmin(store); e != nil || ready {
		problem(w, 409, "目标数据库已初始化或凭据异常，不能覆盖")
		return
	}
	if err = archive.SaveBootstrap(s.path, c); err != nil {
		problem(w, 500, "无法保存启动配置，请检查文件权限")
		return
	}
	s.cfg = c
	if err = store.Initialize(c, record); err != nil {
		// Keep the destination pointer on a failed/ambiguous commit so restart can
		// inspect the actual database, instead of opening a different setup wizard.
		problem(w, 500, err.Error())
		return
	}

	s.completed = true
	respond(w, 200, map[string]bool{"ok": true})
	s.done <- c
}
