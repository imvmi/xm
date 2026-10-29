# 自动构建与发布

[返回项目首页](../README.md)

GitHub Actions 的 [ci 工作流](../.github/workflows/ci.yml) 在推送 Git tag 时自动构建并发布。普通分支推送和 Pull Request 仅运行测试，不创建 Release；也可手动指定已有 tag 补发。

## 构建产物

| 目标 | 压缩包 |
| --- | --- |
| Windows x64 | `xm-windows-amd64.zip` |
| macOS Intel | `xm-darwin-amd64.tar.gz` |
| macOS Apple Silicon | `xm-darwin-arm64.tar.gz` |
| Linux x64 | `xm-linux-amd64.tar.gz` |
| Linux ARM64 | `xm-linux-arm64.tar.gz` |

Go 版本读取 `go.mod`。全部版本使用 `CGO_ENABLED=0` 交叉编译，压缩包内含程序、README 和详细文档，不打包本地配置、数据库或 Cookie。ARM 目标为 64 位，不包含 ARMv7。

管理页面内置在二进制中，运行仍需要本机 Chrome / Chromium 或 CDP 远程浏览器。macOS 产物未签名、未公证；此流程只验证交叉编译成功，不等同于在各目标系统上完成运行测试。

## 发布规则

- Go 测试、`go vet`、前端测试和全部目标构建通过后才发布。
- 使用推送的 tag 作为 Release 标签和版本名，不自动创建新标签。支持普通标签和附注标签。
- Release 不存在时先创建草稿，上传五个压缩包及 `SHA256SUMS`，全部成功后才公开。失败时保留草稿供重试。
- 不自动取消其他标签的构建，每个标签独立发布。
- 同名 Release 已存在时仍上传附件，同名附件会被替换，标题和说明保留。已有草稿在上传成功后公开；已公开的 Release 直接补齐附件。
- 工作流只在发布作业授予 `contents: write`，使用 GitHub 自动提供的 `GITHUB_TOKEN`，无需额外保存个人 Token。

例如发布 `v1.0.0`（在已提交的代码上执行）：

```bash
git tag -a v1.0.0 -m "Release v1.0.0"
git push github v1.0.0
```

示例使用本项目的 `github` 远端；如果你的 GitHub 远端名为 `origin`，请相应替换。仅在本地创建 tag 不触发 CI，需要将 tag 推送到 GitHub。失败后可在 **Actions → ci** 查看对应作业日志，并选择 **Re-run failed jobs**。若组织策略禁用 Actions 或禁止写入仓库，需要先由管理员允许此工作流运行和发布。

### 给已有 tag 补发

在 **Actions → ci → Run workflow** 中选择 `main`，在 `tag` 输入框填写已有标签（如 `v1.0.0`），再运行。流程使用所选分支中的工作流定义，测试和二进制构建则检出该 tag 的代码，Release 附件与 tag 保持一致。

修复 CI 后给旧 tag 补发，应使用此入口；直接重跑旧运行仍会使用旧版工作流。无需删除或移动 tag，也无需删除已有 Release。

发布机制参考 [GitHub 工作流权限说明](https://docs.github.com/en/actions/reference/workflows-and-actions/workflow-syntax#permissions) 和 [GitHub CLI Release 文档](https://cli.github.com/manual/gh_release_create)。

## 本地构建

在装有 Go、Bash、Python 3 和 tar 的 Linux 环境中执行：

```bash
bash .github/scripts/package-release.sh windows amd64
bash .github/scripts/package-release.sh darwin amd64
bash .github/scripts/package-release.sh darwin arm64
bash .github/scripts/package-release.sh linux amd64
bash .github/scripts/package-release.sh linux arm64
```

默认输出到 `dist/releases/`，可用第三个参数指定目录。Linux 上可用 `sha256sum -c SHA256SUMS` 校验下载的全部压缩包；macOS 可用 `shasum -a 256 -c SHA256SUMS`。
