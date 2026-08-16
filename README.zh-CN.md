# panpack

[English](README.md)

`panpack` 是一个面向大目录和低磁盘空间机器的可恢复百度网盘备份工具。它增量打包数据，根据实时资源调整卷大小和上传并发，校验远端内容，并原子保存断点状态。

项目使用[百度网盘官方 Go SDK](https://github.com/baidu-netdisk/baidu-drive-sdk-go)及当前的 `Precreate -> SliceUpload -> CreateFile` 协议。

## 核心能力

- 有界内存扫描，不在内存中保存完整目录树。
- 按磁盘余量动态选择卷大小并保留安全空间。
- 并发同时受 cgroup 内存、CPU quota 和用户上限约束。
- 普通文件使用标准 tar；超过单卷上限的文件自动生成可恢复片段。
- 快照、断点状态和运行回执均原子落盘。
- 校验整文件、API 分片和百度复合校验值。
- 远端验证通过后才删除本地文件。
- 支持旧管线封装包的远端精确集合终验。
- 同名远端内容不一致时拒绝覆盖。

## 安装

从 [GitHub Releases](https://github.com/MayMistery/panpack/releases) 下载二进制，或使用 Go 1.26.2 及以上版本安装：

```bash
go install github.com/MayMistery/panpack/cmd/panpack@latest
```

Release 提供 Linux/macOS、amd64/arm64 构建。

## 授权

panpack 不内置应用密钥或用户 token。在百度网盘开放平台创建应用后执行设备登录：

```bash
export BAIDU_APP_KEY='your-app-key'
export BAIDU_SECRET_KEY='your-secret-key'
panpack auth login
```

凭据默认保存到 `~/.config/panpack/credentials.json`，权限为 `0600`。也可以导入现有 bypy 登录态：

```bash
panpack auth import-bypy --from ~/.bypy/bypy.json
```

还可使用 `PANPACK_ACCESS_TOKEN` 或 `PANPACK_TOKEN_FILE`。不要把 token 放进命令参数、仓库或日志。

## 原生备份

先检查资源策略：

```bash
panpack doctor --path /data
```

只生成快照和计划，不上传：

```bash
panpack plan \
  --source /data \
  --remote-dir /apps/your-app/server-backup
```

开始或恢复备份：

```bash
panpack backup \
  --source /data \
  --remote-dir /apps/your-app/server-backup
```

状态目录默认是 `<source>/.panpack`。每卷开始前重新检查资源；封卷上传并通过远端验证后，才原子提交状态并释放本地空间。

## 上传已有封装包

如果旧管线已经生成不可变归档，使用 `upload-batch` 原地续传，不重新打包：

```bash
panpack upload-batch \
  --source-dir /data/staging \
  --pattern 'chunk_*.tar' \
  --remote-dir /apps/your-app/server-backup \
  --state-file /data/upload-state.json \
  --delete-after-verify
```

首次运行会冻结匹配文件的名称和大小。每个文件上传前计算整文件与分片校验；远端同名文件只有在大小和校验一致时才会被接纳，否则立即停止。`--delete-after-verify` 只删除已经通过远端元数据验证的本地包。中断后用相同参数和 state file 重跑即可。

## 内置最终审计

`audit-batch` 一次性验证：状态已完成、远端名称集合完全一致、冻结文件大小与校验一致、本地文件已清空，以及运行回执为成功退出。

验证 `chunk_0000.tar` 到 `chunk_0318.tar`：

```bash
panpack audit-batch \
  --state-file /data/upload-state.json \
  --remote-pattern 'chunk_*.tar' \
  --expected-template 'chunk_%04d.tar' \
  --expected-start 0 \
  --expected-end 318 \
  --require-local-empty \
  --require-checksum \
  --json
```

任意名称集合可通过 `--expected-list FILE` 提供，每行一个 basename。不指定 template 或 list 时，以冻结批次自身作为远端精确集合。

v0.1.4 之前的 state 不含删除后独立复核所需的百度复合校验值；审计旧 state 时省略 `--require-checksum`。旧任务没有运行回执时，可用 `--receipt-file -` 跳过回执检查。

## 持久化运行回执

`backup` 和 `upload-batch` 默认写入原子 JSON 回执，包含命令、PID、开始/结束时间、终态、退出码、state 路径和最终 state SHA-256。

- `status=succeeded` 且 `exit_code=0` 表示命令正常成功返回。
- 可处理的失败会记录 `status=failed` 和非零退出码。
- 进程被强杀时无法伪造成功，回执会停留在 `running`，审计因此失败。
- state 哈希可阻止旧回执验证被修改过的状态文件。

批量上传默认写到 `<state-file>.receipt.json`。可用 `--receipt-file FILE` 指定路径，或用 `--receipt-file -` 关闭。

## 资源策略

默认磁盘保留量是 `max(4 GiB, 文件系统容量 * 5%)`。自动卷大小最多使用剩余可用空间的一半，为已封卷和正在处理的数据同时留出空间。

上传并发最多从 4 开始；无重试完成后逐步增加，遇到频控或重试压力时降低。最终上限由 cgroup 可用内存、CPU quota 和 `--max-upload-concurrency` 共同决定。

常用参数：

- `--volume-size auto`：自动选择安全卷大小，也可指定 `1GiB` 等固定值。
- `--min-free 4GiB`：保留的绝对磁盘空间。
- `--reserve-fraction 0.05`：按文件系统容量保留的比例。
- `--max-upload-concurrency 16`：自适应上传并发硬上限。
- `--exclude-name NAME`：按 basename 排除，可重复使用。

远端路径必须位于 `/apps/<app>/...`。

## 恢复

下载同一快照的 snapshot、压缩 manifest、index 和全部 volume 后执行：

```bash
panpack restore \
  --snapshot snapshot-<id>.json \
  --manifest manifest-<id>.jsonl.gz \
  --volumes ./downloaded-volumes \
  --destination ./restored
```

恢复器拒绝绝对路径、`..` 穿越和经由符号链接父目录写出目标目录。超过单卷上限的文件使用 panpack 片段，必须通过该命令重组。

## 保证与边界

- 打包时复核源文件大小、mode 和 mtime；快照后的修改会停止任务。
- 状态与封卷使用 write、sync、原子 rename 的提交顺序。
- 保存普通文件、目录、符号链接、Unix mode 和 mtime。
- v1 不保存 ACL、xattr、owner、稀疏布局或硬链接关系。
- 路径必须是 UTF-8。
- 默认不压缩、不加密，以适应低 CPU 环境。
- 当前不提供百度网盘批量下载；请先使用官方或兼容客户端下载，再执行恢复。

旧 bypy 管线说明见 [docs/MIGRATION.md](docs/MIGRATION.md)。

## 开发

```bash
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath ./cmd/panpack
```

## License

Apache-2.0。百度网盘官方 Go SDK 同样使用 Apache-2.0。
