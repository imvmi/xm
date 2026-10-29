# XM · 个人 X 动态归档

把关注的 X（Twitter）账号动态保存到自己的设备或服务器，随时通过中文网页浏览、搜索和收藏。

XM 使用你自己的 X 登录 Cookie 采集内容，支持公开账号，以及当前登录账号有权查看的锁推账号。

## 核心功能

- **自动归档**：定时采集推文、图片、视频和引用内容，支持历史补采、断点续采及失败重试。
- **时间线浏览**：按账号查看动态、回复和媒体，支持搜索、筛选、收藏、图片放大和视频播放。
- **显示设置**：日间 / 夜间主题，可调整每次加载数量、引用显示层数（0～4 层）及视频播放偏好。
- **灵活存储**：数据库支持 SQLite、MySQL、MariaDB；媒体支持本地磁盘、Samba、WebDAV 和 S3。
- **消息通知**：通过 Telegram 或 Webhook 接收新动态、采集异常与恢复通知。
- **外部 API**：为调用方创建独立密钥，按操作和账号范围授权，查询归档或提交采集任务。

## 快速开始

### 1. 下载程序

在 [GitHub Releases](https://github.com/imvmi/xm/releases) 下载适合系统的压缩包并解压：

| 系统 | 文件 |
| --- | --- |
| Windows x64 | `xm-windows-amd64.zip` |
| macOS Intel | `xm-darwin-amd64.tar.gz` |
| macOS Apple Silicon | `xm-darwin-arm64.tar.gz` |
| Linux x64 | `xm-linux-amd64.tar.gz` |
| Linux ARM64 | `xm-linux-arm64.tar.gz` |

推送 Git tag 后，CI 会运行检查、编译上述版本，通过后以该 tag 发布 Release，并附上 `SHA256SUMS` 校验文件。Linux ARM 版本面向 64 位系统。macOS 二进制暂未签名或公证。

运行需要 **Chrome / Chromium**（也可连接 CDP 远程浏览器）、可访问 X 的网络，以及自己的 X 登录 Cookie。管理页面已内置，无需安装 Go 或 Node.js。

### 2. 启动并初始化

Linux / macOS 在解压目录运行：

```bash
./xm serve
```

Windows 在 PowerShell 中运行：

```powershell
.\xm.exe serve
```

打开 [http://127.0.0.1:8080](http://127.0.0.1:8080)，按向导选择数据库、媒体存储并创建管理员账号。第一次使用建议选择 **SQLite + 本地存储**。

设置完成后自动生成 `config.yaml`，无需手写配置。使用 MySQL / MariaDB 时需先创建数据库；远程存储需准备好目录、共享或存储桶。

### 3. 开始采集

1. 登录后进入 **设置 → 认证**，导入 X Cookie。可从已登录 X 的浏览器开发者工具中取得 `auth_token` 和 `ct0`，支持以下 JSON 格式：

   ```json
   {"auth_token": "你的 auth_token", "ct0": "你的 ct0"}
   ```

2. 在 **设置 → 爬虫** 配置代理（如需），点击“测试 Chrome 与 Cookie”检查连接。
3. 在 **账号** 页面添加用户名并开启自动采集；需要该账号发出的回复时，开启“包含回复”。
4. 在 **任务** 页面查看进度，采集完成后到首页或账号时间线浏览。

## 日常使用

| 想做什么 | 在哪里操作 |
| --- | --- |
| 搜索、筛选、收藏推文 | 首页或账号时间线 |
| 添加账号、暂停采集、补采历史 | 账号 |
| 查看采集进度和失败原因 | 任务 |
| 更新 Cookie、代理和采集周期 | 设置 → 认证 / 爬虫 |
| 调整主题、引用显示层数和视频播放 | 设置 → 时间线 |
| 管理数据库、媒体存储和数据迁移 | 设置 → 数据 |
| 配置 Telegram / Webhook | 通知 |
| 创建和管理 API 密钥 | 设置 → 凭据 |

API 使用 `/api/v1` 路径和 `Authorization: Bearer YOUR_API_KEY` 认证。启动服务后访问 **`/docs/api`** 查看完整接口文档。

## 从源码运行

安装 Go 1.26+，然后执行：

```bash
git clone https://github.com/imvmi/xm.git
cd xm
go build -o xm ./cmd/xm
./xm serve
```

Windows 编译时改用 `go build -o xm.exe ./cmd/xm`，运行 `.\xm.exe serve`。

## 部署与数据保管

- 首次设置只允许本机访问。服务器可先通过 SSH 隧道完成设置；远程使用建议配置 HTTPS。详见[服务器部署](docs/usage-guide.md#服务器部署)。
- 升级前停止服务并备份 **配置、数据库和媒体文件**，替换程序后按原命令启动。同一数据库只运行一个 XM 实例。
- Cookie 和存储凭据属于敏感信息，部分以明文保存在数据库中；请保护数据目录和备份，不要提交到 Git。
- XM 只能保存当前账号有权访问且 X 实际返回的内容，不保证获取完整历史。登录失效、限流或 X 页面变化都可能中断采集。

## 更多说明

- [详细使用指南](docs/usage-guide.md)：部署、命令行、存储配置、迁移与故障排查。
- [数据库结构](docs/database.md)：数据组织与备份说明。
- [自动发布流程](docs/releases.md)：构建目标、发布规则与手动重试。
