package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"xm/internal/archive"
	"xm/internal/web"
)

const usage = `XM — 个人 X 采集验证程序（Go + SQLite/MySQL + Chrome）

命令：
  xm init          [-config config.yaml]
  xm import-cookie [-config config.yaml] -file cookies.txt
  xm collect       [-config config.yaml] -users alice,bob [-limit 100] [-replies] [-backfill]
  xm retry-media   [-config config.yaml] [-user alice] [-verify]
  xm status        [-config config.yaml]
  xm migrate-mysql -config mysql-config.yaml -source data/xm.db
  xm list          [-config config.yaml] [-user alice] [-limit 20]
  xm serve         [-config config.yaml] [-listen 127.0.0.1:8080] [-secure-cookie]

collect 对每个账号最多采集 limit 条，图片和最高码率 MP4 保存到配置的本地、WebDAV 或 S3 存储。
Cookie 文件支持 Cookie 请求头、JSON 映射/数组、Netscape；-file - 从标准输入导入。
retry-media -verify 同时检查已下载文件的 SHA-256，丢失或损坏的文件重新下载。
不同 X 登录账号请使用不同的配置文件和 data_dir。
`

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	for {
		err := run(ctx, os.Args[1:])
		if errors.Is(err, web.ErrMigrationRestart) && ctx.Err() == nil {
			continue
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "错误:", err)
			os.Exit(1)
		}
		return
	}
}

func run(ctx context.Context, args []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return nil
	}
	command := args[0]
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := f.String("config", "config.yaml", "配置文件")
	var file, users, user, source *string
	var limit *int
	var replies, verify, backfill *bool
	var listen *string
	var secure *bool
	switch command {
	case "init", "status":
	case "migrate-mysql":
		source = f.String("source", "", "待迁移的 SQLite 文件；目标 MySQL 必须为空")
	case "serve":
		listen = f.String("listen", "127.0.0.1:8080", "管理页面监听地址")
		secure = f.Bool("secure-cookie", false, "HTTPS 反向代理部署时启用 Secure 会话 Cookie")
	case "import-cookie":
		file = f.String("file", "", "Cookie 文件路径；- 表示标准输入")
	case "collect":
		users = f.String("users", "", "用户名或主页 URL，多个用逗号分隔")
		limit = f.Int("limit", 100, "每个账号的最大推文数")
		replies = f.Bool("replies", false, "打开推文与回复页面")
		backfill = f.Bool("backfill", false, "禁用增量边界，按上限继续向历史滚动")
	case "retry-media":
		user = f.String("user", "", "只重试此目标的媒体")
		verify = f.Bool("verify", false, "校验已下载文件并修复")
	case "list":
		user = f.String("user", "", "筛选目标账号")
		limit = f.Int("limit", 20, "最多显示条数")
	default:
		return fmt.Errorf("未知命令 %q；运行 xm help 查看用法", command)
	}
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("存在多余参数；参数必须使用 -users、-file 等选项指定")
	}
	if command == "init" {
		cfg := archive.Bootstrap{DataDir: "./data", Database: archive.DatabaseConfig{Driver: "sqlite"}}
		h, err := os.OpenFile(*configPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("创建配置失败（已有文件不会覆盖）: %w", err)
		}
		b, encodeErr := archive.MarshalBootstrap(cfg)
		if encodeErr != nil {
			h.Close()
			return encodeErr
		}
		_, err = h.Write(b)
		closeErr := h.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Println("已创建", *configPath, "；运行 serve 并打开管理页面，按首次设置向导完成初始化。")
		return nil
	}
	cfg, err := archive.LoadConfig(*configPath)
	if command == "serve" && os.IsNotExist(err) {
		// A fresh deployment needs no configuration file. Setup writes bootstrap only
		// after the database and storage have been selected and tested.
		cfg = archive.DefaultConfig()
		cfg.DataDir, err = filepath.Abs(filepath.Join(filepath.Dir(*configPath), "data"))
	}
	if err != nil {
		return err
	}
	if command == "serve" || command == "collect" || command == "retry-media" || command == "import-cookie" || command == "migrate-mysql" {
		release, e := archive.AcquireWorkerLock(cfg.DataDir)
		if e != nil {
			return e
		}
		defer release()
	}
	if command == "serve" {
		cfg, err = web.Setup(ctx, *configPath, cfg, *listen, *secure)
		if err != nil {
			return err
		}
	}
	if command == "migrate-mysql" {
		if !cfg.Database.IsMySQL() || *source == "" {
			return errors.New("迁移需要 mysql 或 mariadb 配置及 -source SQLite 文件")
		}
		sourceDir, e := filepath.Abs(filepath.Dir(*source))
		if e != nil {
			return e
		}
		if sourceDir != cfg.DataDir {
			release, e := archive.AcquireWorkerLock(sourceDir)
			if e != nil {
				return e
			}
			defer release()
		}
	}
	s, err := archive.OpenConfiguredStore(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	if command == "serve" || command == "collect" || command == "retry-media" || command == "import-cookie" || command == "migrate-mysql" {
		release, e := s.AcquireDatabaseWorker(ctx, cancel, cfg.Database.Name)
		if e != nil {
			return e
		}
		defer release()
	}
	if command == "import-cookie" {
		if *file == "" {
			return errors.New("请使用 -file 指定 Cookie 文件")
		}
		var r io.Reader = os.Stdin
		if *file != "-" {
			h, e := os.Open(*file)
			if e != nil {
				return e
			}
			defer h.Close()
			r = h
		}
		data, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
		if err != nil {
			return errors.New("读取 Cookie 文件失败")
		}
		if len(data) > 1<<20 {
			return errors.New("Cookie 文件超过 1 MiB")
		}
		cookies, err := archive.ParseCookies(data)
		if err != nil {
			return err
		}
		if err = s.SaveCookies(cookies); err != nil {
			return err
		}
		if err = web.CookieImported(cfg); err != nil {
			return err
		}
		fmt.Println("已将 auth_token 和 ct0 明文保存到数据库；格式检查通过，实际登录与访问权限将在 collect 时验证。")
		return nil
	}
	if limit != nil && (*limit < 1 || *limit > 10000) {
		return errors.New("limit 必须在 1～10000 之间")
	}
	if user != nil && *user != "" {
		normalized, e := archive.Username(*user)
		if e != nil {
			return e
		}
		*user = normalized
	}
	var targets []string
	if command == "collect" {
		if *users == "" {
			return errors.New("请通过 -users 指定监控账号")
		}
		seen := map[string]bool{}
		for _, raw := range strings.Split(*users, ",") {
			name, e := archive.Username(raw)
			if e != nil {
				return e
			}
			if !seen[name] {
				targets = append(targets, name)
				seen[name] = true
			}
		}
	}

	switch command {
	case "migrate-mysql":
		if err = web.MigrateStore(s.DB); err != nil {
			return errors.New("创建 MySQL 管理表失败，请检查数据库权限")
		}
		counts, err := s.CopySQLiteToMySQL(ctx, *source)
		if err != nil {
			return err
		}
		fmt.Println(archive.JSON(counts))
		fmt.Println("迁移完成，源 SQLite 未修改。Cookie、管理员凭据和运行配置已随数据库迁移；请保留原 data_dir 中的本地媒体。")
		return nil
	case "serve":
		return web.Serve(ctx, *configPath, cfg, s, *listen, *secure)
	case "status":
		v, e := s.Status()
		if e != nil {
			return e
		}
		fmt.Println(archive.JSON(v))
		return nil
	case "list":
		v, e := s.Export(*user, *limit)
		if e != nil {
			return e
		}
		fmt.Println(archive.JSON(v))
		return nil
	case "retry-media":
		v, e := archive.DownloadPending(ctx, s, cfg, *user, *verify)
		fmt.Println(archive.JSON(v))
		return e
	case "collect":
		cookies, e := s.LoadCookies()
		if e != nil {
			return e
		}
		var mediaFailed bool
		for _, target := range targets {
			fmt.Fprintf(os.Stderr, "开始采集 @%s（最多 %d 条）\n", target, *limit)
			id, e := s.StartRun(target)
			if e != nil {
				return e
			}
			identity, e := s.TargetIdentity(target)
			if e != nil {
				return e
			}
			if !*backfill {
				identity.KnownPostIDs, e = s.KnownTargetPosts(target, max(*limit*4, 500))
				if e != nil {
					return e
				}
			}
			var latestProfile *archive.Profile
			result, collectErr := archive.CollectBound(ctx, cfg, cookies, target, identity, *limit, *replies, func(page archive.Page) error {
				if page.Profile != nil {
					if err := s.SaveTargetProfile(target, *page.Profile); err != nil {
						return err
					}
					profile := *page.Profile
					latestProfile = &profile
				}
				if err := s.SavePosts(target, page.Posts); err != nil {
					return err
				}
				if len(page.Posts) > 0 {
					fmt.Fprintf(os.Stderr, "@%s 已入库本页 %d 条\n", target, len(page.Posts))
				}
				return nil
			})
			if latestProfile != nil && latestProfile.AvatarURL != "" && ctx.Err() == nil {
				_ = archive.DownloadAvatar(ctx, cfg, *latestProfile)
			}
			status, message := "bounded_complete", result.Stop
			if collectErr != nil {
				status = "failed"
				message = collectErr.Error()
			}
			if e = s.EndRun(id, status, result.Posts, message); e != nil {
				return e
			}
			// Text and media jobs are committed before attempting downloads. Even a
			// partial collection remains usable and retryable after browser failure.
			downloads, downloadErr := archive.DownloadPending(ctx, s, cfg, target, false)
			fmt.Println(archive.JSON(map[string]any{"target": target, "collection": result, "media": downloads}))
			if collectErr != nil {
				return collectErr
			}
			if downloadErr != nil {
				fmt.Fprintln(os.Stderr, downloadErr)
				mediaFailed = true
			}
		}
		if mediaFailed {
			return errors.New("文字采集已完成，但有媒体未成功归档；请运行 retry-media")
		}
	}
	return nil
}
