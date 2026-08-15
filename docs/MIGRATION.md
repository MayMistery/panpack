# 从 bypy + shell/Python 管线迁移

旧管线常见结构是：先扫描文件并生成固定清单，再用 `tar -T` 打卷，最后并行运行 `bypy upload`。这种方法在目标卷大小为 1.9 GB 时仍有一个关键缺口：tar 不能把单个文件拆到两个独立归档中，因此只要源文件本身超过目标值，最终 tar 仍会超过 2 GiB并退回 `bypy` 分片上传；如果旧 PCS 分片接口返回 `Slice MD5 mismatch`，这些卷会永久卡住。

panpack 的迁移原则：

1. 不导入旧的 `done/` 标记作为新快照完成证据，因为旧标记只表示脚本认为成功，且不同代脚本的失败语义不一致。
2. 可以导入 `~/.bypy/bypy.json` 作为临时访问凭据，但不会复制 hash cache、parts cache 或日志。
3. 新备份使用独立快照 ID 和远端文件名；不覆盖旧 tar 卷。
4. 普通文件保持 tar 兼容；超大文件由 panpack 自己分片并由恢复命令重组。
5. 只有百度 API 创建文件后的响应以及随后目录元数据中的 size/MD5 都匹配，卷才进入完成状态。

建议首次迁移：

```bash
panpack auth import-bypy

panpack plan \
  --source /root/autodl-tmp \
  --state-dir /root/autodl-tmp/.panpack \
  --remote-dir /apps/bypy/autodl-tmp-backup-panpack

panpack backup \
  --source /root/autodl-tmp \
  --state-dir /root/autodl-tmp/.panpack \
  --remote-dir /apps/bypy/autodl-tmp-backup-panpack
```

不要把旧的 `.bypy` 目录、token 文件、源文件清单或运行日志提交到 GitHub。
