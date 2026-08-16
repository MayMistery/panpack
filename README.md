# panpack

[Simplified Chinese](README.zh-CN.md)

`panpack` is a resumable Baidu Netdisk backup tool for large directory trees and storage-constrained machines. It packages data incrementally, adapts volume size and upload concurrency to current resources, verifies remote content, and persists enough state to resume safely after interruption.

It uses the [official Baidu Netdisk Go SDK](https://github.com/baidu-netdisk/baidu-drive-sdk-go) and the current `Precreate -> SliceUpload -> CreateFile` protocol.

## Highlights

- Bounded-memory directory scanning; the full tree is never retained in memory.
- Adaptive volume sizing with explicit disk reserves.
- Upload concurrency bounded by cgroup memory, CPU quota, and a user-defined ceiling.
- Standard tar entries for ordinary files and restorable fragments for files larger than one volume.
- Atomic snapshots, resume state, and run receipts.
- Whole-file, block, and Baidu multipart checksum verification.
- Local files are deleted only after verified remote completion.
- Exact-set remote audits for sealed legacy batches.
- Collision refusal when an existing remote name has different content.

## Installation

Download a binary from [GitHub Releases](https://github.com/MayMistery/panpack/releases), or install from source with Go 1.26.2 or later:

```bash
go install github.com/MayMistery/panpack/cmd/panpack@latest
```

Release artifacts are provided for Linux and macOS on amd64 and arm64.

## Authentication

panpack never embeds application secrets or user tokens. Create an application in the Baidu Netdisk developer console, then use device login:

```bash
export BAIDU_APP_KEY='your-app-key'
export BAIDU_SECRET_KEY='your-secret-key'
panpack auth login
```

Credentials are stored at `~/.config/panpack/credentials.json` with mode `0600`.

An existing bypy session can also be imported:

```bash
panpack auth import-bypy --from ~/.bypy/bypy.json
```

Imported bypy credentials often lack the application keys required for refresh. Re-authenticate when they expire. Credentials may also be supplied through `PANPACK_ACCESS_TOKEN` or `PANPACK_TOKEN_FILE`; never place tokens in command arguments, repositories, or logs.

## Native backup

Inspect the resource policy without modifying data:

```bash
panpack doctor --path /data
```

Build a snapshot and execution plan without uploading:

```bash
panpack plan \
  --source /data \
  --remote-dir /apps/your-app/server-backup
```

Start or resume the backup:

```bash
panpack backup \
  --source /data \
  --remote-dir /apps/your-app/server-backup
```

State defaults to `<source>/.panpack`. The command re-evaluates resource limits before each volume, uploads and verifies one sealed volume, commits its state atomically, then releases the local space.

## Uploading an existing sealed batch

Use `upload-batch` when another pipeline has already produced immutable archives and they must not be repacked:

```bash
panpack upload-batch \
  --source-dir /data/staging \
  --pattern 'chunk_*.tar' \
  --remote-dir /apps/your-app/server-backup \
  --state-file /data/upload-state.json \
  --delete-after-verify
```

The first run freezes the matching basenames and sizes in the state file. Every archive is hashed before upload. An existing remote file is accepted only when its size and checksum match; a collision stops the run. `--delete-after-verify` removes each local archive only after remote metadata verification. Resume with the same command and state file.

## Exact final audit

`audit-batch` turns final verification into one machine-readable command. It verifies completed state, the exact remote name set, frozen-file sizes and checksums, optional local cleanup, and the successful run receipt.

For a numbered set from `chunk_0000.tar` through `chunk_0318.tar`:

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

For arbitrary names, pass a newline-delimited basename file with `--expected-list FILE`. If neither an expected template nor list is supplied, the frozen batch itself is treated as the exact expected set.

States created before v0.1.4 do not contain the multipart checksum required for an independent post-deletion checksum audit. Omit `--require-checksum` for those states; upload-time verification and frozen size checks remain available. Use `--receipt-file -` only when auditing a legacy run that predates receipts.

## Durable run receipts

`backup` and `upload-batch` create an atomic JSON receipt by default. It records the command, PID, start and finish times, terminal status, exit code, state-file path, and the final state SHA-256.

- `status=succeeded` and `exit_code=0` prove a normal successful return.
- A handled failure records `status=failed` and a non-zero exit code.
- A hard kill cannot forge success: the receipt remains `running` and the audit fails.
- The state hash prevents a stale receipt from validating a modified state file.

The default batch receipt is `<state-file>.receipt.json`. Override it with `--receipt-file FILE`, or disable it with `--receipt-file -`.

## Resource policy

The default disk reserve is `max(4 GiB, filesystem size * 5%)`. Automatic volume sizing uses at most half of the remaining usable space, capped to the configured range, so a sealed volume and in-progress work can coexist safely.

Upload concurrency starts at no more than four workers. It increases after clean uploads and decreases under retry or rate-limit pressure. The ceiling is constrained by available cgroup memory, CPU quota, and `--max-upload-concurrency`.

Common controls:

- `--volume-size auto` selects a safe volume size; fixed values such as `1GiB` are supported.
- `--min-free 4GiB` reserves an absolute amount of free space.
- `--reserve-fraction 0.05` reserves a fraction of filesystem capacity.
- `--max-upload-concurrency 16` caps adaptive upload workers.
- `--exclude-name NAME` excludes an exact basename and may be repeated.

Remote paths must be under `/apps/<app>/...`.

## Restore

Download the snapshot JSON, compressed manifest, index, and every volume for the same snapshot, then run:

```bash
panpack restore \
  --snapshot snapshot-<id>.json \
  --manifest manifest-<id>.jsonl.gz \
  --volumes ./downloaded-volumes \
  --destination ./restored
```

The restorer rejects absolute paths, `..` traversal, and writes through symlink parents. Files larger than one volume use panpack fragments and must be reconstructed with this command.

## Guarantees and limits

- Source size, mode, and modification time are checked again while packing; post-snapshot changes stop the run.
- State and sealed-volume commits use write, sync, and atomic rename ordering.
- Regular files, directories, symbolic links, Unix mode, and modification time are preserved.
- ACLs, extended attributes, ownership, sparse layout, and hard-link relationships are not preserved in v1.
- Paths must be valid UTF-8.
- Compression and encryption are intentionally out of scope for the low-CPU default pipeline.
- Bulk download from Baidu Netdisk is not implemented; use an official or compatible download client before restore.

See the [legacy bypy migration notes](docs/MIGRATION.md) for the original shell/Python workflow.

## Development

```bash
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath ./cmd/panpack
```

## License

Apache-2.0. The official Baidu Netdisk Go SDK is also licensed under Apache-2.0.
