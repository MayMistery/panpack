# panpack

`panpack` 是一个面向低磁盘空间、大目录和百度网盘限速场景的可恢复备份工具。它使用 Go 实现扫描、分卷、状态恢复、并发控制、远端校验和恢复流程，并通过[百度网盘官方 Go SDK](https://github.com/baidu-netdisk/baidu-drive-sdk-go)调用当前的 `Precreate → SliceUpload → CreateFile` 上传协议。

它解决的核心问题不是“把目录 tar 一下”，而是让数百 GB、数百万文件的任务在 2 GB 内存、低 CPU 和紧张磁盘下仍然可重跑、可验证：

- 扫描目录时按批读取，不把整棵文件树放进内存。
- 每卷前重新检测磁盘、cgroup 内存和 CPU quota，动态选择卷大小。
- 普通文件写成标准 tar 条目；单个文件大于卷上限时自动切成可恢复片段，因此不会突破卷大小。
- 只保留至多一个正在处理的本地卷；服务端 size/MD5 校验成功后才删除。
- 百度 API 分片并发会在成功后逐步增加，遇到重试或频控则自动减半。
- 快照清单、卷状态和上传状态都原子落盘；进程中断后运行同一命令即可继续。
- 打包时再次校验源文件的 mode、mtime 和 size；快照后发生变化会停止，避免静默生成混合时点备份。

## 安装

下载 GitHub Release 中对应平台的压缩包，或使用 Go 1.26.2 以上版本安装：

```bash
go install github.com/MayMistery/panpack/cmd/panpack@latest
```

Release 提供 Linux amd64/arm64 和 macOS amd64/arm64 二进制。

## 百度授权

公开发布的工具不会内置 AppKey、SecretKey 或任何用户 token。推荐在百度网盘开放平台创建应用后使用设备码登录：

```bash
export BAIDU_APP_KEY='your-app-key'
export BAIDU_SECRET_KEY='your-secret-key'
panpack auth login
```

凭据默认保存到 `~/.config/panpack/credentials.json`，权限为 `0600`。也可以迁移现有 `bypy` 登录态：

```bash
panpack auth import-bypy --from ~/.bypy/bypy.json
```

迁移的 `bypy` 文件通常只有 token，没有应用密钥，因此不能由 panpack 自动刷新；失效后需重新授权。还可通过 `PANPACK_ACCESS_TOKEN` 或 `PANPACK_TOKEN_FILE` 提供凭据。不要把 token 放在命令行参数、仓库或日志里。

## 使用

先查看当前机器会选择的资源策略：

```bash
panpack doctor --path /data
```

生成本地快照清单和执行计划，不上传：

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

重要参数：

- `--state-dir`：清单和断点状态目录，默认 `<source>/.panpack`。
- `--staging-dir`：当前 tar 卷目录，默认 `<state-dir>/staging`。
- `--volume-size auto`：默认自动计算；也可固定为 `512MiB`、`1GiB` 等。
- `--min-free 4GiB`：无论如何都保留的最小空间。
- `--reserve-fraction 0.05`：额外保留文件系统总容量的比例；实际取两者较大值。
- `--max-upload-concurrency 16`：自适应并发的硬上限。
- `--exclude-name NAME`：按 basename 排除，参数可重复；默认不会擅自排除 `.git`、虚拟环境等业务内容。

远端路径必须位于 `/apps/<app>/...`。卷文件使用快照 ID 命名，因此多次备份不会互相覆盖；`index-<snapshot>.json` 最后上传，可作为快照提交标记。

## 恢复

从网盘下载同一快照的以下文件：

- `snapshot-<id>.json`
- `manifest-<id>.jsonl.gz`
- `index-<id>.json`
- 所有 `<id>.volume-*.tar`

恢复命令可直接读取 `.jsonl.gz` 清单：

```bash
panpack restore \
  --snapshot snapshot-<id>.json \
  --manifest manifest-<id>.jsonl.gz \
  --volumes ./downloaded-volumes \
  --destination ./restored
```

普通文件是标准 tar 条目；只有超过卷上限的文件使用 `.panpack/fragments/` 条目，必须由 `panpack restore` 重组。恢复器会拒绝绝对路径、`..` 路径和经由符号链接父目录写出目标目录的情况。

## 自适应策略

默认磁盘保留量为 `max(4 GiB, 文件系统总容量 × 5%)`。每卷开始前重新计算：

```text
可用于管线的空间 = 当前可用空间 - 保留量
卷上限 = clamp(可用于管线的空间 / 2, 64 MiB, 2 GiB)
```

`/2` 为“已封卷 + 正在写入卷”保留安全预算，即使当前版本采用更保守的逐卷上传，也不会在失败重试时把磁盘顶满。

上传并发同时受三项约束：用户上限、cgroup/系统可用内存、CPU quota。初始最多 4；一个卷零重试完成后加 1，出现频控或较多重试后减半。网络上传按每核最多 16 个 I/O worker 设硬上限，因此 0.5 核机器可从 4 自适应增长到 8。每个 API 分片默认 4 MiB，官方 SDK 要求 multipart 请求带准确 `Content-Length`，因此内存预算按每 worker 三个分片缓冲加网络开销估算。

## 一致性与断点

状态提交顺序是：

1. 扫描并原子提交不可变 JSONL 快照清单。
2. 写入 `.tmp` 卷，`fsync` 后重命名为已封卷。
3. 原子记录卷的 MD5、API block MD5 和下一个清单游标。
4. 上传、合并并通过远端 size/MD5 校验。
5. 原子标记已上传，再删除本地卷。

任何一步被杀掉都可以安全重跑；已在远端成功但未及时记状态的卷会通过秒传/覆盖和远端校验收敛。

## 当前边界

- 默认不压缩、不加密，以适应低 CPU；如需加密应在上传前使用独立、经过审计的加密层。
- 保存普通文件、目录、符号链接、Unix mode 和 mtime；v1 不保存 ACL、xattr、owner、稀疏布局或硬链接关系。
- 路径必须是 UTF-8。
- 运行期间修改源文件会使当前任务失败；重新建立快照时请使用新的 state 目录。
- 当前只实现上传与本地恢复；从百度网盘批量下载可使用官方客户端或其他下载工具。

## 开发

```bash
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build ./cmd/panpack
```

详细的旧管线迁移说明见 [docs/MIGRATION.md](docs/MIGRATION.md)。

## License

Apache-2.0。百度网盘官方 Go SDK 同样采用 Apache-2.0。
