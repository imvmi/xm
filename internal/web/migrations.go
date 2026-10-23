package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"xm/internal/archive"
)

var ErrMigrationRestart = errors.New("restart with migrated database")

type migrationFailure struct {
	PostID  string `json:"post_id"`
	MediaID string `json:"media_id"`
	Message string `json:"message"`
}
type migrationJob struct {
	ID              string                  `json:"id"`
	Kind            string                  `json:"kind"`
	Source          string                  `json:"source"`
	Target          string                  `json:"target"`
	TargetIdentity  string                  `json:"target_identity,omitempty"`
	Database        archive.DatabaseConfig  `json:"-"`
	Storage         *archive.StorageProfile `json:"-"`
	Status          string                  `json:"status"`
	Acknowledged    bool                    `json:"acknowledged,omitempty"`
	Phase           string                  `json:"phase"`
	Done            int64                   `json:"done"`
	Total           int64                   `json:"total"`
	Bytes           int64                   `json:"bytes"`
	TotalBytes      int64                   `json:"total_bytes"`
	Failed          int64                   `json:"failed"`
	Failures        []migrationFailure      `json:"failures"`
	Message         string                  `json:"message"`
	TargetFile      string                  `json:"target_file,omitempty"`
	BackupDirectory string                  `json:"backup_directory,omitempty"`
	UpdatedAt       int64                   `json:"updated_at"`
}
type migrationManager struct {
	app     *App
	mu      sync.Mutex
	job     *migrationJob
	running bool
	closing bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func newMigrationManager(a *App) (*migrationManager, error) {
	m := &migrationManager{app: a}
	b, err := os.ReadFile(m.journal())
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, errors.New("无法读取迁移进度文件")
	}
	var j migrationJob
	if json.Unmarshal(b, &j) != nil || j.ID == "" {
		return nil, errors.New("迁移进度文件损坏，请检查备份")
	}
	m.job = &j
	if j.Target == "" { // Journals written before bidirectional migration.
		if j.Kind == "database" {
			j.Target = "sqlite"
		} else {
			j.Target = "local"
		}
	}
	if j.Status == "running" || j.Status == "switching" {
		cfg := a.Config()
		installed := j.Target == "sqlite" && cfg.Database.DriverName() == "sqlite" && cfg.Database.SQLiteFile == j.TargetFile
		installed = installed || (migrationRemoteDatabase(j.Target) && cfg.Database.DriverName() == j.Target && j.TargetIdentity == migrationDatabaseIdentity(cfg.Database))
		if j.Kind == "database" && installed {
			// A rename that succeeded but whose directory sync failed must not
			// permit new archive writes until bootstrap is durably installed.
			if err = syncDirectory(filepath.Dir(a.configPath)); err != nil {
				return nil, errors.New("迁移启动配置尚未同步，请检查磁盘后重启")
			}
			j.Status = "completed"
			j.Phase = "完成"
			j.Message = "目标数据库已启用；源数据库保留，原启动配置备份在 " + j.BackupDirectory + "/bootstrap-before.yaml。"
		} else {
			j.Status = "paused"
			j.Phase = "已中断"
			j.Message = "服务曾中断；请手动继续。媒体已完成项不会重复搬迁，数据库将从新快照重试。"
		}
		if err = m.persistLocked(); err != nil {
			return nil, err
		}
	}
	return m, nil
}
func (m *migrationManager) journal() string {
	return filepath.Join(m.app.Config().DataDir, "migration-state.json")
}
func (m *migrationManager) persistLocked() error {
	m.job.UpdatedAt = time.Now().Unix()
	b, err := json.Marshal(m.job)
	if err != nil {
		return err
	}
	if err = archive.AtomicWrite(m.journal(), b); err != nil {
		return errors.New("无法保存迁移进度，请检查磁盘空间和权限")
	}
	return syncDirectory(filepath.Dir(m.journal()))
}
func syncDirectory(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return archive.SyncDirectory(f)
}
func (m *migrationManager) update(fn func(*migrationJob)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m.job)
	return m.persistLocked()
}
func (m *migrationManager) snapshot() *migrationJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job == nil {
		return nil
	}
	j := *m.job
	j.Failures = append([]migrationFailure{}, j.Failures...)
	return &j
}
func (m *migrationManager) stop() {
	m.mu.Lock()
	m.closing = true
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
}

type migrationPreview struct {
	Kind          string `json:"kind"`
	Source        string `json:"source"`
	Target        string `json:"target"`
	Items         int64  `json:"items"`
	Bytes         int64  `json:"bytes"`
	FreeBytes     uint64 `json:"free_bytes"`
	RequiredBytes int64  `json:"required_bytes"`
	Destination   string `json:"destination"`
	Warning       string `json:"warning"`
}

type migrationInput struct {
	Kind     string                  `json:"kind"`
	Source   string                  `json:"source"`
	Target   string                  `json:"target"`
	Database archive.DatabaseConfig  `json:"database"`
	Storage  *archive.StorageProfile `json:"storage,omitempty"`
	Confirm  string                  `json:"confirm"`
	Retry    bool                    `json:"retry"`
}

func migrationRemoteDatabase(driver string) bool {
	return driver == "mysql" || driver == "mariadb"
}

func migrationDatabaseIdentity(c archive.DatabaseConfig) string {
	port := c.Port
	if port == 0 {
		port = 3306
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d/%s", c.Host, port, c.Name))))
}

func (a *App) prepareMigration(input *migrationInput) error {
	cfg := a.Config()
	switch input.Kind {
	case "cleanup":
		input.Source, input.Target = "local", "remote"
		input.Storage = nil
		input.Database = archive.DatabaseConfig{}
	case "database":
		input.Storage = nil
		if input.Source != "" && input.Source != cfg.Database.DriverName() {
			return errors.New("源数据库已变化，请重新检查迁移条件")
		}
		input.Source = cfg.Database.DriverName()
		if input.Target == "" {
			if migrationRemoteDatabase(input.Source) {
				input.Target = "sqlite"
			} else {
				input.Target = "mysql"
			}
		}
		if !((input.Source == "sqlite" && migrationRemoteDatabase(input.Target)) || (migrationRemoteDatabase(input.Source) && input.Target == "sqlite")) {
			return errors.New("请选择另一种数据库作为迁移目标")
		}
		if migrationRemoteDatabase(input.Target) {
			input.Database.Driver = input.Target
			input.Database.SQLiteFile = ""
			if input.Database.CAFile != "" && !filepath.IsAbs(input.Database.CAFile) {
				input.Database.CAFile = filepath.Join(filepath.Dir(a.configPath), input.Database.CAFile)
			}
			return input.Database.Validate()
		}
	case "media":
		input.Database = archive.DatabaseConfig{}
		if input.Source == "" {
			input.Source = cfg.ActiveStorage()
		}
		if input.Target == "" {
			input.Target = "local"
		}
		if input.Source == input.Target || (input.Source != "local" && input.Target != "local") {
			return errors.New("请选择本地与远端之间的迁移方向")
		}
		if cfg.ActiveStorage() != input.Source && cfg.ActiveStorage() != input.Target {
			return errors.New("请先迁移当前正在使用的存储")
		}
		if input.Source != "local" {
			input.Storage = nil
			p, err := cfg.Storage(input.Source)
			if err != nil || (p.Kind != "s3" && p.Kind != "webdav" && p.Kind != "smb") {
				return errors.New("请选择已配置的远端存储")
			}
		}
		if input.Target != "local" {
			if input.Target == "new" {
				if input.Storage == nil {
					return errors.New("请填写目标远端存储配置")
				}
				input.Storage.ID = "migration_new"
				if input.Storage.Timeout == 0 {
					input.Storage.Timeout = 120
				}
				if err := input.Storage.Validate(); err != nil {
					return err
				}
			} else {
				p, err := cfg.Storage(input.Target)
				if err != nil {
					return err
				}
				if err = p.Validate(); err != nil {
					return err
				}
				input.Storage = nil
			}
		}
	default:
		return errors.New("不支持的迁移类型")
	}
	return nil
}

func (a *App) previewMigration(ctx context.Context, input migrationInput) (migrationPreview, error) {
	cfg := a.Config()
	p := migrationPreview{Kind: input.Kind, Source: input.Source, Target: input.Target, Destination: cfg.DataDir}
	freeBytes, err := availableDiskBytes(cfg.DataDir)
	if err != nil {
		return p, errors.New("无法检查本地磁盘可用空间")
	}
	p.FreeBytes = freeBytes
	switch input.Kind {
	case "cleanup":
		if err := a.store.DB.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(bytes),0) FROM media_local_copies WHERE storage_id<>'local'").Scan(&p.Items, &p.Bytes); err != nil {
			return p, errors.New("无法读取本地副本记录")
		}
		if p.Items == 0 {
			return p, errors.New("没有已记录的迁移副本可清理")
		}
		p.Destination = "已迁出的本地媒体副本"
		p.Warning = "清理期间暂停写入。仅处理迁移留下的本地副本；逐个重新读取远端并校验大小和 SHA256，确认本地内容相同且未被归档引用后删除。校验失败的文件保留；数据库、远端文件和在用本地媒体不会删除。"
	case "database":
		if err := a.store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM posts").Scan(&p.Items); err != nil {
			return p, errors.New("无法读取推文数量")
		}
		// The engine estimate can lag; retain a conservative floor and headroom.
		if migrationRemoteDatabase(input.Target) {
			var pages, size int64
			if err := a.store.DB.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
				return p, errors.New("无法估算 SQLite 大小")
			}
			if err := a.store.DB.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
				return p, errors.New("无法估算 SQLite 大小")
			}
			p.Bytes = pages * size
			dst, err := archive.OpenMigrationMySQL(ctx, input.Database)
			if err != nil {
				return p, err
			}
			defer dst.Close()
			if err = dst.CheckMigrationEmpty(ctx); err != nil {
				return p, err
			}
			p.Destination = input.Database.DisplayName() + " · " + input.Database.Host + " / " + input.Database.Name
			p.RequiredBytes = 1 << 20
			p.Warning = "维护期间暂停采集、通知和设置写入；复制全部数据并逐表校验后自动重启，需要重新登录。目标必须为空的 " + input.Database.DisplayName() + " 数据库，请确保远端有足够的数据和事务日志空间。源 SQLite 和原启动配置保留。"
			break
		}
		if err := a.store.DB.QueryRowContext(ctx, "SELECT COALESCE(SUM(data_length+index_length),0) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&p.Bytes); err != nil {
			return p, errors.New("无法估算数据库大小")
		}
		p.RequiredBytes = max(p.Bytes*3, 256<<20)
		p.Warning = "维护期间暂停采集、通知和设置写入；等待当前任务结束后复制并逐表校验，随后重启应用，需要重新登录。原 " + cfg.Database.DisplayName() + " 和原启动配置保留，不覆盖已有 SQLite 文件。空间为估算值。"
	case "media":
		if err := a.store.DB.QueryRowContext(ctx, "SELECT COUNT(*),COALESCE(SUM(bytes),0) FROM media WHERE storage_id=? AND status='done'", input.Source).Scan(&p.Items, &p.Bytes); err != nil {
			return p, errors.New("无法读取媒体统计")
		}
		p.RequiredBytes = p.Bytes + (64 << 20)
		p.Warning = "等待当前采集任务结束后，将新媒体保存到服务器本地；旧文件在后台逐个下载并校验。失败项保留远端引用，可手动重试。源文件保留；下载可能产生流量费用。"
		if input.Target != "local" {
			profile, _ := cfg.Storage(input.Target)
			if input.Storage != nil {
				profile = *input.Storage
			}
			p.Destination = profile.Name
			p.RequiredBytes = 1 << 20
			p.Warning = "等待当前采集任务结束后，将新媒体保存到目标远端；历史本地文件逐个上传并回读校验后切换引用。源文件保留，失败项仍由本地提供，可手动重试。请确保远端空间和读写权限充足；上传与回读可能产生流量费用。"
		}
	default:
		return p, errors.New("不支持的迁移类型")
	}
	if uint64(p.RequiredBytes) > p.FreeBytes {
		return p, errors.New("本地磁盘空间不足，请先扩容或释放空间")
	}
	return p, nil
}
func (a *App) migrationStatus(w http.ResponseWriter, r *http.Request) {
	profiles := []map[string]string{}
	for _, p := range a.Config().StorageProfiles {
		if p.Kind == "s3" || p.Kind == "webdav" || p.Kind == "smb" {
			profiles = append(profiles, map[string]string{"id": p.ID, "name": p.Name, "kind": p.Kind})
		}
	}
	var copies, copyBytes int64
	if err := a.store.DB.QueryRowContext(r.Context(), "SELECT COUNT(*),COALESCE(SUM(bytes),0) FROM media_local_copies WHERE storage_id<>'local'").Scan(&copies, &copyBytes); err != nil {
		problem(w, 500, "无法读取本地副本统计")
		return
	}
	job := a.migrations.snapshot()
	if job != nil {
		switch job.Kind {
		case "media", "database", "cleanup":
		default:
			// Retired tools may leave a journal, but must not expose unusable
			// progress or retry controls after an upgrade.
			job = nil
		}
	}
	respond(w, 200, map[string]any{"local_copies": map[string]int64{"items": copies, "bytes": copyBytes}, "job": job, "database_driver": a.Config().Database.DriverName(), "active_storage": a.Config().ActiveStorage(), "sources": profiles, "maintenance": a.maintenance.Load()})
}
func (a *App) migrationPreview(w http.ResponseWriter, r *http.Request) {
	input := migrationInput{Kind: r.URL.Query().Get("kind"), Source: r.URL.Query().Get("source"), Target: r.URL.Query().Get("target")}
	if r.Method == "POST" && !decode(w, r, &input) {
		return
	}
	if err := a.prepareMigration(&input); err != nil {
		problem(w, 400, err.Error())
		return
	}
	p, err := a.previewMigration(r.Context(), input)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	respond(w, 200, p)
}
func (a *App) startMigration(w http.ResponseWriter, r *http.Request) {
	var input migrationInput
	if !decode(w, r, &input) {
		return
	}
	if input.Confirm != "开始迁移" && input.Confirm != "清理本地副本" {
		problem(w, 400, "请输入“开始迁移”确认")
		return
	}
	m := a.migrations
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running || m.closing || (m.job != nil && m.job.Status == "switching") {
		problem(w, 409, "已有迁移正在执行或服务正在停止")
		return
	}
	if input.Retry {
		if m.job == nil || (m.job.Status != "paused" && m.job.Status != "failed") {
			problem(w, 409, "没有可重试的迁移")
			return
		}
		input.Kind, input.Source, input.Target = m.job.Kind, m.job.Source, m.job.Target
		if input.Kind == "media" && input.Target != "local" {
			if _, err := a.Config().Storage(input.Target); err != nil {
				input.Target = "new"
			}
		}
	}
	if (input.Kind == "cleanup") != (input.Confirm == "清理本地副本") {
		problem(w, 400, "确认文字与操作不匹配")
		return
	}
	if err := a.prepareMigration(&input); err != nil {
		problem(w, 400, err.Error())
		return
	}
	p, err := a.previewMigration(r.Context(), input)
	if err != nil {
		problem(w, 400, err.Error())
		return
	}
	// Retry uses a fresh snapshot / remaining remote references, not stale counts.
	j := &migrationJob{ID: rand.Text(), Kind: input.Kind, Source: p.Source, Target: p.Target, Database: input.Database, Storage: input.Storage, Status: "running", Phase: "等待当前任务结束", Total: p.Items, TotalBytes: p.Bytes, Failures: []migrationFailure{}}
	if j.Target == "new" {
		j.Target = "migration_" + j.ID
		j.Storage.ID = j.Target
	}
	if j.Kind == "database" && migrationRemoteDatabase(j.Target) {
		j.TargetIdentity = migrationDatabaseIdentity(j.Database)
	}
	if j.Kind == "database" && j.Target == "sqlite" {
		j.TargetFile, err = migrationSQLiteFilename(a.Config().DataDir, j.ID)
		if err != nil {
			problem(w, 500, "无法检查本地数据库目标文件")
			return
		}
	}
	previous := m.job
	m.job = j
	if err = m.persistLocked(); err != nil {
		m.job = previous
		problem(w, 500, err.Error())
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.running = true
	a.maintenance.Store(true)
	a.scheduler.Wake()
	a.notifier.Wake()
	m.wg.Add(1)
	go m.run(ctx, *j)
	respond(w, http.StatusAccepted, j)
}
func (a *App) pauseMigration(w http.ResponseWriter, r *http.Request) {
	m := a.migrations
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running {
		problem(w, 409, "没有执行中的迁移")
		return
	}
	if m.job.Status == "switching" {
		problem(w, 409, "正在切换数据库，请等待重启完成")
		return
	}
	m.cancel()
	respond(w, 200, map[string]string{"message": "已请求暂停，正在安全结束当前步骤"})
}
func (a *App) acknowledgeMigration(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &input) {
		return
	}
	m := a.migrations
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running || m.closing || m.job == nil || m.job.Status != "completed" || input.ID == "" || input.ID != m.job.ID {
		problem(w, 409, "迁移状态已变化，请刷新后确认已完成的任务")
		return
	}
	if !m.job.Acknowledged {
		// Keep the journal for recovery/auditing; acknowledgement only hides its UI.
		previous := *m.job
		m.job.Acknowledged = true
		if err := m.persistLocked(); err != nil {
			*m.job = previous
			problem(w, 500, err.Error())
			return
		}
	}
	respond(w, 200, map[string]string{"message": "已确认完成，迁移进度已隐藏"})
}
func (m *migrationManager) run(ctx context.Context, j migrationJob) {
	defer m.wg.Done()
	a := m.app
	// All HTTP mutations, a scheduler batch and notifier delivery hold RLock.
	// The maintenance flag rejects new writers before we wait for old ones.
	locked := false
	ticker := time.NewTicker(50 * time.Millisecond)
	for ctx.Err() == nil {
		if a.writeGate.TryLock() {
			locked = true
			break
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	ticker.Stop()
	err := ctx.Err()
	if err == nil && j.Kind == "database" {
		err = m.copyDatabase(ctx, j)
	}
	if err == nil && j.Kind == "cleanup" {
		err = m.cleanupLocalCopies(ctx)
	}
	if err == nil && j.Kind == "media" {
		a.changeMu.Lock()
		cfg := a.Config()
		cfg.MediaStorage = j.Target
		if j.Storage != nil {
			cfg.StorageProfiles = append(append([]archive.StorageProfile{}, cfg.StorageProfiles...), *j.Storage)
		}
		if j.Target != "local" {
			err = m.update(func(current *migrationJob) { current.Phase = "验证目标存储连接" })
			if err == nil {
				err = newSetup(a.configPath, cfg).checkStorage(ctx, cfg)
			}
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = a.store.SaveSettings(cfg)
		}
		if err == nil {
			a.configMu.Lock()
			a.config = cfg
			a.configMu.Unlock()
		}
		a.changeMu.Unlock()
	}
	// Successful database cutover must remain read-only until all old workers
	// stop. A new App is constructed from bootstrap by the serve restart loop.
	switching := err == nil && j.Kind == "database"
	if !switching {
		a.maintenance.Store(false)
	}
	if locked {
		a.writeGate.Unlock()
	}
	if err == nil && j.Kind == "media" {
		err = m.copyMedia(ctx, j)
	}
	if !switching {
		finishErr := m.update(func(current *migrationJob) {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				current.Status = "paused"
				current.Phase = "已暂停"
				current.Message = "已完成项保留；可手动继续，源数据未删除。"
				if j.Kind == "cleanup" {
					current.Message = "已清理项不会恢复，剩余副本保留；可继续清理。"
				}
			} else if err != nil {
				current.Status = "failed"
				current.Phase = "失败"
				current.Message = err.Error()
			} else {
				current.Status = "completed"
				current.Phase = "完成"
				current.Message = "迁移及校验完成；源文件保留，新媒体保存到目标存储。"
				if j.Kind == "cleanup" {
					current.Message = "本地副本清理完成；远端媒体和本地数据库保留。"
				}
			}
		})
		if finishErr != nil {
			m.mu.Lock()
			m.job.Status = "failed"
			m.job.Message = "无法保存最终进度；请检查磁盘后重试，已完成数据保留。"
			m.mu.Unlock()
		}
	}
	m.mu.Lock()
	m.running = false
	m.cancel = nil
	m.mu.Unlock()
	if switching {
		select {
		case a.restart <- struct{}{}:
		default:
		}
	}
}

// Prefer the standard filename, but never overwrite an existing database or
// adopt a stale journal. The final hardlink also enforces no-clobber at commit.
func migrationSQLiteFilename(dir, id string) (string, error) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Lstat(filepath.Join(dir, archive.DefaultSQLiteFile+suffix)); err == nil {
			return "xm-migrated-" + id + ".db", nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
	}
	return archive.DefaultSQLiteFile, nil
}

func (m *migrationManager) copyDatabase(ctx context.Context, j migrationJob) error {
	a := m.app
	cfg := a.Config()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Staging is unique, and destination installation refuses to overwrite.
	dir, err := os.MkdirTemp(cfg.DataDir, "migration-db-")
	if err != nil {
		return errors.New("无法创建数据库迁移目录")
	}
	old, err := os.ReadFile(a.configPath)
	if err != nil {
		return errors.New("无法备份启动配置")
	}
	if err = archive.AtomicWrite(filepath.Join(dir, "bootstrap-before.yaml"), old); err != nil {
		return errors.New("无法备份启动配置")
	}
	if err = syncDirectory(dir); err != nil {
		return errors.New("无法同步启动配置备份")
	}
	if err = m.update(func(j *migrationJob) { j.BackupDirectory = filepath.Base(dir) }); err != nil {
		return err
	}
	var dst *archive.Store
	if migrationRemoteDatabase(j.Target) {
		connection, e := archive.OpenMigrationMySQL(ctx, j.Database)
		if e != nil {
			return e
		}
		defer connection.Close()
		release, e := connection.AcquireDatabaseWorker(ctx, cancel, j.Database.Name)
		if e != nil {
			return e
		}
		defer release()
		if e = connection.CheckMigrationEmpty(ctx); e != nil {
			return e
		}
		targetCfg := cfg
		targetCfg.Database = j.Database
		dst, err = archive.OpenConfiguredStore(targetCfg)
	} else {
		dst, err = archive.OpenStore(dir)
	}
	if err != nil {
		return errors.New("无法创建目标数据库，请检查权限与连接配置")
	}
	defer dst.Close()
	if err = migrate(dst); err != nil {
		return errors.New("无法建立目标数据库表结构")
	}
	err = a.store.CopyDatabase(ctx, dst, func(phase string, done, total int64) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return m.update(func(j *migrationJob) { j.Phase = phase; j.Done = done; j.Total = total })
	})
	if err != nil {
		return err
	}
	targetCfg := cfg
	if migrationRemoteDatabase(j.Target) {
		targetCfg.Database = j.Database
	} else {
		targetCfg.Database = archive.DatabaseConfig{Driver: "sqlite", SQLiteFile: j.TargetFile}
	}
	if _, err = dst.LoadSettings(targetCfg); err != nil {
		return errors.New("目标数据库运行配置校验失败")
	}
	if _, ready, e := loadAdmin(dst); e != nil || !ready {
		return errors.New("目标数据库管理员凭据校验失败")
	}
	if j.Target == "sqlite" {
		if _, err = dst.DB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return errors.New("无法提交 SQLite 日志")
		}
		if err = dst.Close(); err != nil {
			return errors.New("关闭 SQLite 失败")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		target := filepath.Join(cfg.DataDir, j.TargetFile)
		if err = os.Link(filepath.Join(dir, archive.DefaultSQLiteFile), target); err != nil {
			return errors.New("无法安装 SQLite 文件（不会覆盖现有文件）")
		}
		if err = syncDirectory(cfg.DataDir); err != nil {
			return errors.New("SQLite 目录同步失败")
		}
		// Do not retain a second filename for the live SQLite inode: its future WAL
		// will be named after target, so opening an old hardlink would be unsafe.
		if err = os.Remove(filepath.Join(dir, archive.DefaultSQLiteFile)); err != nil {
			return errors.New("无法移除 SQLite 暂存别名，尚未切换数据库")
		}
		if err = syncDirectory(dir); err != nil {
			return errors.New("无法同步 SQLite 暂存目录")
		}
	}
	// Serialize cancellation with the point of no return. Journal is durable
	// before bootstrap changes; after a crash startup reconciles via TargetFile.
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	m.job.Status = "switching"
	m.job.Phase = "切换数据库并重启"
	m.job.Message = "校验通过；原启动配置位于 " + filepath.Base(dir) + "/bootstrap-before.yaml。"
	if err = m.persistLocked(); err != nil {
		return err
	}
	if err = archive.SaveBootstrap(a.configPath, targetCfg); err != nil {
		return errors.New("切换启动配置失败，原数据库仍在使用")
	}
	// Rename has succeeded: never resume writes to the old database, even if
	// directory fsync fails. Restart reconciles the persisted bootstrap choice.
	_ = syncDirectory(filepath.Dir(a.configPath))
	return nil
}
func (m *migrationManager) copyMedia(ctx context.Context, j migrationJob) error {
	a := m.app
	cfg := a.Config()
	rows, err := a.store.DB.QueryContext(ctx, "SELECT post_id,media_id,storage_id,object_key,local_path,bytes,sha256 FROM media WHERE storage_id=? AND status='done' ORDER BY post_id,media_id", j.Source)
	if err != nil {
		return errors.New("无法读取待迁移媒体")
	}
	var items []archive.MigrationMedia
	var totalBytes int64
	for rows.Next() {
		var item archive.MigrationMedia
		if err = rows.Scan(&item.PostID, &item.MediaID, &item.StorageID, &item.Key, &item.LocalPath, &item.Bytes, &item.SHA256); err != nil {
			break
		}
		items = append(items, item)
		totalBytes += item.Bytes
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return errors.New("读取媒体索引失败")
	}
	verb := "下载"
	if j.Target != "local" {
		verb = "上传"
	}
	if err = m.update(func(j *migrationJob) {
		j.Phase = verb + "并校验媒体"
		j.Total = int64(len(items))
		j.TotalBytes = totalBytes
		j.Done = 0
		j.Bytes = 0
	}); err != nil {
		return err
	}
	var failed int64
	for _, item := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = m.update(func(j *migrationJob) {
			j.Phase = verb + "并校验推文 " + item.PostID + " 的媒体 " + item.MediaID
		}); err != nil {
			return err
		}
		if j.Target == "local" {
			err = a.store.LocalizeMedia(ctx, cfg, item)
		} else {
			err = a.store.RemoteizeMedia(ctx, cfg, item, j.Target)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			failed++
		}
		e := m.update(func(j *migrationJob) {
			if err == nil {
				j.Done++
				j.Bytes += item.Bytes
			} else {
				j.Failed++
				if len(j.Failures) < 100 {
					j.Failures = append(j.Failures, migrationFailure{item.PostID, item.MediaID, err.Error()})
				}
			}
		})
		if e != nil {
			return e
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d 个文件迁移失败；源存储引用保留。修复原因后可重试剩余文件。", failed)
	}
	return nil
}
