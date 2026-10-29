# 自动构建与发布

[返回项目首页](../README.md)

GitHub Actions 的 [ci 工作流](../.github/workflows/ci.yml) 在每次推送到 `main` 时自动构建并发布。其他分支和 Pull Request 仅运行原有测试，不创建 Release。

## 构建产物

| 目标 | 压缩包 |
| --- | --- |
| Windows x64 | `xm-windows-amd64.zip` |
| macOS Intel | `xm-darwin-amd64.tar.gz` |
| macOS Apple Silicon | `xm-darwin-arm64.tar.gz` |
| Linux x64 | `xm-linux-amd64.tar.gz` |
| Linux ARM64 | `xm-linux-arm64.tar.gz` |

Go 版本读取 `go.mod`。全部版本使用 `CGO_ENABLED=0` 交叉编译，压缩包内含程序、README、详细文档和部署脚本，不打包本地配置、数据库或 Cookie。ARM 目标为 64 位，不包含 ARMv7。

管理页面内置在二进制中，运行仍需要本机 Chrome / Chromium 或 CDP 远程浏览器。macOS 产物未签名、未公证；此流程只验证交叉编译成功，不等同于在各目标系统上完成运行测试。

## 发布规则

- Go 测试、`go vet`、前端测试、MariaDB 集成测试和全部目标构建通过后才发布。
- 每次运行使用独立标签 `main-<运行编号>-<提交短 SHA>`，准确指向触发构建的提交。
- 先创建草稿、上传五个压缩包及 `SHA256SUMS`，全部成功后才公开。失败时保留草稿供重试。
- 每次主分支推送都保留自己的构建，不自动取消旧运行；发布时仍对应当前主分支的构建才标记为 Latest。
- 重跑同一工作流时复用原标签，继续未完成的草稿；已发布的 Release 保持原样。
- 工作流只在发布作业授予 `contents: write`，使用 GitHub 自动提供的 `GITHUB_TOKEN`，无需额外保存个人 Token。

在仓库 **Actions → ci → Run workflow** 选择 `main` 可手动触发。失败后可查看对应作业日志，并选择 **Re-run failed jobs**。若组织策略禁用 Actions 或禁止写入仓库，需要先由管理员允许此工作流运行和发布。

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
