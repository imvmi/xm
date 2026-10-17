package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"xm/internal/archive"
	"xm/internal/web"
)

const usage = `XM — 个人 X 采集验证程序（Go + SQLite + Chrome）

命令：
  xm init          [-config config.json]
  xm import-cookie [-config config.json] -file cookies.txt
  xm collect       [-config config.json] -users alice,bob [-limit 100] [-replies]
  xm retry-media   [-config config.json] [-user alice] [-verify]
  xm status        [-config config.json]
  xm list          [-config config.json] [-user alice] [-limit 20]
  xm serve         [-config config.json] [-listen 127.0.0.1:8080] [-secure-cookie]

collect 对每个账号最多采集 limit 条，图片和最高码率 MP4 保存到本地。
Cookie 文件支持 Cookie 请求头、JSON 映射/数组、Netscape；-file - 从标准输入导入。
retry-media -verify 同时检查已下载文件的 SHA-256，丢失或损坏的文件重新下载。
不同 X 登录账号请使用不同的配置文件和 data_dir。
`

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return nil
	}
	command := args[0]
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := f.String("config", "config.json", "配置文件")
	var file, users, user *string
	var limit *int
	var replies, verify *bool
	var listen *string
	var secure *bool
	switch command {
	case "init", "status":
	case "serve":
		listen = f.String("listen", "127.0.0.1:8080", "管理页面监听地址")
		secure = f.Bool("secure-cookie", false, "HTTPS 反向代理部署时启用 Secure 会话 Cookie")
	case "import-cookie":
		file = f.String("file", "", "Cookie 文件路径；- 表示标准输入")
	case "collect":
		users = f.String("users", "", "用户名或主页 URL，多个用逗号分隔")
		limit = f.Int("limit", 100, "每个账号的最大推文数")
		replies = f.Bool("replies", false, "打开推文与回复页面")
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
		cfg := archive.Config{DataDir: "./data", Headless: true, RequestTimeout: 60, CollectionTimeout: 300, ScrollDelay: 3, MaxScrolls: 40, MaxMediaBytes: 512 << 20, DefaultInterval: 900}
		h, err := os.OpenFile(*configPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("创建配置失败（已有文件不会覆盖）: %w", err)
		}
		_, err = h.WriteString(archive.JSON(cfg) + "\n")
		closeErr := h.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Println("已创建", *configPath, "；按需设置 proxy 和 chrome_path，再导入 Cookie。")
		return nil
	}
	cfg, err := archive.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if command == "serve" || command == "collect" || command == "retry-media" || command == "import-cookie" {
		release, e := archive.AcquireWorkerLock(cfg.DataDir)
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
		if err = archive.SaveCookies(cfg.DataDir, cookies); err != nil {
			return err
		}
		if err = web.CookieImported(cfg.DataDir); err != nil {
			return err
		}
		fmt.Println("已加密保存 auth_token 和 ct0；格式检查通过，实际登录与访问权限将在 collect 时验证。")
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
	s, err := archive.OpenStore(cfg.DataDir)
	if err != nil {
		return err
	}
	defer s.Close()
	if command == "serve" || command == "collect" || command == "retry-media" {
		if err := s.BackfillQuotes(); err != nil {
			return err
		}
	}
	switch command {
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
		cookies, e := archive.LoadCookies(cfg.DataDir)
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
			result, collectErr := archive.Collect(ctx, cfg, cookies, target, *limit, *replies, func(page archive.Page) error {
				if page.Profile != nil {
					if err := s.SaveProfile(*page.Profile); err != nil {
						return err
					}
				}
				if err := s.SavePosts(target, page.Posts); err != nil {
					return err
				}
				if len(page.Posts) > 0 {
					fmt.Fprintf(os.Stderr, "@%s 已入库本页 %d 条\n", target, len(page.Posts))
				}
				return nil
			})
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
