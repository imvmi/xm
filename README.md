# XM：个人 X 动态归档

Go + SQLite 的自部署归档服务，内置中文管理页面，也保留命令行采集工具。使用你自己的 Cookie 和独立的临时 Chrome，读取目标主页实际返回的时间线数据。支持公开账号和**当前登录账号已获准关注**的锁推账号；不提供越权访问、自动关注、自动登录或验证码处理。

支持 Cookie 导入、代理配置、账号管理、定时轮询，以及推文、图片和视频浏览。每轮仍是有限条数的采集，媒体保存在本地，尚未接入 S3。网页结构变化、登录验证和限流仍可能中断采集。使用网页自动化存在 X 规则和账号限制风险。

## 运行条件

- Go 1.26+（启用 Go 自动工具链下载时，旧版 Go 会按 go.mod 下载所需版本）。
- 本机 Chrome / Chromium。通过 `chrome_path` 指定可执行文件；未设置则自动查找。
- 一个能访问 X 的网络或代理，以及你已登录 X 的 `auth_token`、`ct0`。
- 推荐普通用户运行，保留 Chrome 沙箱。Linux ARM64 也可使用系统 Chromium。

不依赖 Python、Node、FFmpeg、Redis 或系统 SQLite；SQLite 驱动是纯 Go。

## 快速开始

```bash
cd /home/zyx/XM
go build -o xm ./cmd/xm
./xm init  # 已有 config.json 时跳过
./xm serve
```

打开 http://127.0.0.1:8080。首次启动会在终端显示随机管理员密码，请妥善保存。登录后，在「连接设置」导入 Cookie、配置代理，再到「监控账号」添加目标。已有命令行采集的数据会直接显示在「推文归档」。

也可以在首次启动前设置环境变量 `XM_ADMIN_PASSWORD`（至少 12 字节）。密码以加盐哈希保存在 `data/admin.json`；创建后环境变量不会覆盖已有密码。登录会话有效期 12 小时，重启服务需要重新登录。忘记密码时，先停止服务，将 `admin.json` 移到受保护的备份位置，再启动生成新密码。

### 管理页面

- **推文归档**：按账号、关键词、媒体类型筛选，分页浏览，图片点击放大，视频直接播放并支持拖动进度。媒体下载失败会显示原因。
- **监控账号**：添加、编辑、暂停、恢复、移除账号；每个账号独立设置周期（1 分钟～7 天）、每轮条数（1～1000）及是否包含回复；支持立即采集、停止当前任务。移除监控保留已归档内容。
- **采集记录**：查看最近 20 次任务的完成状态、读取条数和错误信息。
- **连接设置**：粘贴 Cookie 或选择 JSON / Netscape 文件；配置 HTTP(S) / SOCKS5 代理并测试网络连通性；设置新账号的默认周期。

后台一次执行一个采集任务，完成后才计算下一次时间。全局暂停只停止自动触发，当前任务继续执行，手动采集仍可用。Cookie 失效会阻止后续采集，重新导入后恢复；无权访问会暂停对应账号；X 限流会让全部任务至少冷却 15 分钟；其他失败按周期和指数退避延后重试。服务重启会恢复监控计划，并标记被中断的任务。

页面保存的代理、默认周期和暂停状态写回 `config.json`；Cookie、监控列表和任务状态保存在数据目录。Cookie 或代理更新在下一个任务生效。不要在服务运行期间手动覆盖配置文件。默认周期仅作用于新账号。代理测试只验证网络响应，不验证 Cookie 或锁推访问权限。

`serve`、`collect`、`retry-media`、`import-cookie` 对同一数据目录互斥。服务运行期间请通过页面导入 Cookie 和采集；使用命令行采集/修复前先停止服务。`list`、`status` 可同时读取。

### 服务器部署

默认仅监听本机 `127.0.0.1:8080`。可通过 SSH 转发访问，或放在 HTTPS 反向代理后面：

```bash
./xm serve -config /absolute/path/config.json -listen 127.0.0.1:8080 -secure-cookie
```

`-secure-cookie` 用于浏览器通过 HTTPS 访问的部署；普通本地 HTTP 访问不加此参数。反向代理须保留原始 `Host` 和 `Origin`，把整个站点根路径转发给 XM。需要监听其他接口时可指定 `-listen 0.0.0.0:8080`；远程访问应使用 HTTPS 或 SSH 隧道保护 Cookie、密码和私密内容。服务可由 systemd 等进程管理器保持运行；停止时使用 SIGTERM / Ctrl+C，以便取消采集并保留已保存结果。

编辑生成的 `config.json`。比如使用本机 HTTP 代理：

```json
{
  "data_dir": "./data",
  "proxy": "http://127.0.0.1:7890",
  "chrome_path": "",
  "headless": true,
  "request_timeout_seconds": 60,
  "collection_timeout_seconds": 300,
  "scroll_delay_seconds": 3,
  "max_scrolls": 40,
  "max_media_bytes": 536870912,
  "default_interval_seconds": 900,
  "polling_paused": false
}
```

`proxy` 为空表示明确直连，不继承 `HTTP_PROXY` 等环境变量。支持 `http://`、`https://`、`socks5://`、`socks5h://`，必须填写端口，可包含代理用户名和密码。用户名/密码中的特殊字符需要 URL 编码。配置含密码时保持文件权限 `0600`。

`data_dir` 的相对路径以配置文件所在目录为基准。`request_timeout_seconds` 限制每个媒体下载请求（包括下载正文），大视频可适当提高。`collection_timeout_seconds` 限制每个目标的浏览器采集阶段，不包含后续媒体下载。

### 导入 Cookie

在你已登录 X 的浏览器中，从开发者工具的 Application / Storage → Cookies → `https://x.com` 读取 `auth_token` 和 `ct0`，在本地编辑器里保存为 `cookies.json`：

```json
{
  "auth_token": "替换为你的实际值",
  "ct0": "替换为你的实际值"
}
```

```bash
chmod 600 cookies.json
./xm import-cookie -file cookies.json
```

也支持 `auth_token=...; ct0=...` 格式的 Cookie 请求头、浏览器导出的 JSON 数组，以及 Netscape Cookie 文件。只保留 `auth_token` 和 `ct0`，忽略其他 Cookie。使用 `-file -` 可从标准输入导入。

不要把 Cookie 粘贴到聊天、命令行参数或提交到 Git。导入成功仅代表格式检查通过，不代表登录态在线有效。导入文件仍由你自己保管或移除；程序不会擅自删除它。

### 验证公开与锁推账号

先分别采集少量内容，将下面的占位用户名替换为真实目标：

```bash
./xm collect -users public_user -limit 10
./xm collect -users approved_private_user -limit 10
```

正常后可以指定多个目标：

```bash
./xm collect -users public_user,approved_private_user -limit 100
./xm collect -users approved_private_user -limit 100 -replies
./xm list -user approved_private_user -limit 20
./xm status
```

`-limit` 是**每个目标**本次最多保存的推文数（1～10000），不是历史完整性保证。默认打开用户主时间线；`-replies` 打开推文与回复页面，但只归档目标账号本人发出的内容，不把对话模块中的其他人回复当作该账号的推文。

浏览器使用新的临时配置目录，导入 Cookie 后要求检测到已登录界面；不会主动退回访客模式。采集依赖页面实际返回的数据，不硬编码 GraphQL query ID，不读取你的日常浏览器配置。浏览器退出后清理临时配置；异常断电或强制杀进程可能留下临时目录。

每批推文和媒体任务先在同一个 SQLite 事务内提交，再进入媒体下载阶段。重复采集更新相同推文 ID 的内容，不重复插入，也不重复下载已经成功的媒体。中途失败保留已入库数据，重新运行从主页开始有限扫描、去重；**当前没有持久化网页分页游标**。后台轮询同样从最近内容开始有限扫描；如果两次轮询之间的新内容超过每轮上限，可能遗漏，需缩短周期或提高条数上限。不能保证回填全部历史。

停止原因：

- `limit_reached`：达到本次条数上限。
- `max_scrolls_reached`：达到滚动次数上限。
- `no_new_posts_observed`：连续多轮未观察到新内容，不能据此认定已取得全部历史。

`Protected` 为 `null` 表示本次未读取到目标资料，不能推断为公开账号。无法访问、登录失效、限流或响应结构变化均返回非零退出码。只出现无法访问的占位内容也会报错，不计为成功的空时间线。

### 媒体重试与修复

```bash
./xm retry-media
./xm retry-media -user approved_private_user
./xm retry-media -verify
```

图片请求原图；视频和动图选择响应里最高码率的 MP4。当前不合成 HLS，不递归采集引用推文或外部链接附件；没有 MP4 的媒体会记录失败原因。媒体地址可能过期，必要时先重新采集刷新地址，再重试。

下载使用同一代理，不向媒体 CDN 发送账号 Cookie。只接受 `pbs.twimg.com` 和 `video.twimg.com` 的 HTTPS 地址，重定向也受相同限制。流式下载到私有临时文件，检查大小和响应类型，成功后原子替换；SQLite 保存 SHA-256。`-verify` 会检查已归档文件，补下载丢失或损坏的文件。

## 数据与隐私

```text
data/
  archive.sqlite       推文、资料、媒体任务、监控账号、运行记录及调度状态
  archive.sqlite-wal   运行期间可能存在的 WAL
  archive.sqlite-shm   运行期间可能存在的共享内存文件
  cookies.enc          AES-256-GCM 加密的 Cookie
  cookie.key           独立的密钥文件（0600）
  admin.json           管理员密码的盐和哈希（0600）
  worker.lock          防止多个采集进程同时运行的系统锁
  media/xx/...         以稳定哈希命名的图片和视频（0600）
```

数据目录权限为 `0700`。Cookie 加密可避免直接在文件里看到明文，但密钥也在本机，因此**不防御已经能读取整个数据目录的用户**。SQLite 正文、原始推文 JSON 和媒体本身没有加密；如有需要，应使用磁盘加密。

管理 API、推文数据和媒体接口均要求管理员登录；静态登录页面不包含归档内容。媒体支持 Range 请求，不直接暴露数据目录。不要把 `data/` 放在公开 Web 根目录或公开共享盘。`list` 会将正文输出到当前终端，应注意终端日志与重定向文件的访问权限。

一个数据目录对应一个 X 登录账号。更新同一账号的 Cookie 可重复运行导入命令；切换到另一个 X 账号时使用独立配置和 `data_dir`，避免混淆内容的授权来源。尚未实现源站删除、编辑历史和关注权限变化的后台同步，不能作为永久保留源站删除内容的承诺。

备份建议先停止全部 XM 进程，再复制整个数据目录，并保护备份中的 `cookie.key`。运行中不要只复制主 SQLite 文件而遗漏 WAL。

## 代理实现

浏览器通过临时的 loopback HTTP 代理桥连接配置的上游代理，以支持带认证的 HTTP(S)/SOCKS5。HTTPS 使用 CONNECT 隧道，没有 TLS 解密。媒体下载直接使用配置的上游代理；代理失败不会切换直连。SOCKS5 连接将目标域名交给代理处理。

Docker 中的 `127.0.0.1` 指容器自身；宿主机代理需使用容器可达的地址。当前未提供 Docker 镜像。

## 故障排查

| 现象 | 处理 |
|---|---|
| 无法启动 Chrome | 安装 Chrome/Chromium，设置 `chrome_path`，检查运行用户权限与沙箱 |
| 未检测到已登录界面 | 浏览器确认登录，重新导入两个 Cookie；有图形环境时设 `headless=false` 查看页面 |
| 无权查看锁推 | 确认提供 Cookie 的账号已被批准关注，且未被移除或拉黑 |
| HTTP 429 / code 88 | 等待限流恢复，减少采集量；程序不自动换账号或高频重试 |
| 未收到时间线 / 响应结构变化 | 检查网络与 X 页面是否正常；持续出现则需要适配解析器 |
| 媒体失败 | 查询 `media.last_error`，检查大小/超时上限和代理，再运行 `retry-media` |
| 崩溃后 runs 仍为 running | 表示上次未正常结束；启动管理服务会标记中断任务并恢复计划；不会恢复浏览器游标 |

## 验证

```bash
go test ./...
go vet ./...
XM_BROWSER_TEST=1 go test -race ./...
```

默认测试覆盖管理登录、CSRF、配置持久化、监控增删改、搜索分页、私有媒体与 Range、路径穿越防护、调度串行/退避/恢复/凭据替换，以及 Cookie 格式/加密、长文本/转推/媒体解析、SQLite 去重与事务回滚、下载安全边界、带认证 HTTP 代理隧道与 SOCKS5 远端域名转发。浏览器集成测试启动真实 Chrome，在网络请求发出前拦截并返回**合成响应**，验证获准访问、拒绝访问、Cookie 失效和限流；不向 X 发送测试凭据。

本地自动测试不等于真实 X 登录与锁推访问验收。后者必须用你自己的有效 Cookie 和真实目标执行上面的两次少量采集，核对正文、图片和视频。

实现参考：[Chrome DevTools Protocol](https://chromedevtools.github.io/devtools-protocol/)、[chromedp](https://github.com/chromedp/chromedp)、[SQLite WAL](https://www.sqlite.org/wal.html)。
