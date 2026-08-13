#!/usr/bin/env bash
# migrate-from-rails.sh — migrate a Rails rables production site (SQLite DB +
# S3-hosted ActiveStorage blobs) to the Go rables (SQLite + local disk files).
#
# What it does, in order:
#   1. Snapshot the Rails database (sqlite3 .backup — WAL-safe, Rails may stay up)
#   2. Download every S3 object referenced by active_storage_blobs (aws s3 sync)
#   3. Lay the blobs out under DATA_DIR/files/xx/yy/<key> (ActiveStorage disk
#      layout — exactly what the Go server serves at /files/<key>)
#   4. Run migrate-rails --verify-files: copy all tables into the Go schema and
#      rewrite /rails/active_storage/... + <action-text-attachment> references
#      to /files/<key>
#   5. Print a summary (counts, missing files, next steps)
#
# Idempotent: safe to re-run for catch-up before cutover (rows are INSERT OR
# IGNORE, files already in place are skipped). Do NOT use EXISTING_DB=delete
# on a catch-up run — that wipes what was already migrated.
#
# Required environment:
#   RAILS_DB     path to the Rails production.sqlite3 (used read-only)
#   S3_BUCKET    S3 bucket holding the ActiveStorage blobs
#
# Optional environment:
#   S3_PREFIX    key prefix inside the bucket (storage.yml "prefix:", no slashes)
#   DATA_DIR     Go data dir (default: /var/lib/rables)
#   MIGRATE_BIN  path to migrate-rails (default: ./migrate-rails, else $PATH)
#   S3_DUMP_DIR  download cache dir (default: mktemp -d; keep and reuse it for
#                catch-up runs so aws s3 sync stays incremental)
#   EXISTING_DB  what to do when DATA_DIR/rables.db already exists:
#                  delete = back it up, remove it, migrate into a fresh DB
#                  keep   = incremental catch-up into the existing DB
#                (default: ask interactively; abort when stdin is not a TTY)
#   CHOWN_USER   user to chown -R DATA_DIR to at the end when run as root
#                (default: "rables" if that user exists; "none" disables;
#                numeric uid allowed — use 65532 for the Docker image's
#                nonroot user; see deploy/README.md for Docker hosts)
#   REPORT       where to write the migrate-rails report
#                (default: ./migrate-rails-report-<timestamp>.txt)
#   ASSUME_YES   "yes" skips the plan confirmation prompt
#   AWS_PROFILE / AWS_REGION / AWS_* credentials — picked up by the aws CLI
#
# Example:
#   RAILS_DB=/srv/rails-rables/db/production.sqlite3 \
#   S3_BUCKET=my-rables-uploads \
#   DATA_DIR=/var/lib/rables \
#     ./deploy/migrate-from-rails.sh

set -euo pipefail

TS=$(date +%Y%m%d-%H%M%S)

info() { printf '  %s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

usage() {
  sed -n '2,/^set -euo/p' "$0" | sed 's/^# \{0,1\}//; /^set -euo/d' >&2
  exit 2
}

# ---- configuration --------------------------------------------------------

RAILS_DB="${RAILS_DB:-}"
S3_BUCKET="${S3_BUCKET:-}"
S3_PREFIX="${S3_PREFIX:-}"
DATA_DIR="${DATA_DIR:-/var/lib/rables}"
MIGRATE_BIN="${MIGRATE_BIN:-}"
S3_DUMP_DIR="${S3_DUMP_DIR:-}"
EXISTING_DB="${EXISTING_DB:-}"
CHOWN_USER="${CHOWN_USER:-}"
REPORT="${REPORT:-$PWD/migrate-rails-report-$TS.txt}"
ASSUME_YES="${ASSUME_YES:-no}"

missing=""
[ -n "$RAILS_DB" ]  || missing="$missing RAILS_DB"
[ -n "$S3_BUCKET" ] || missing="$missing S3_BUCKET"
if [ -n "$missing" ]; then
  printf 'ERROR: missing required environment:%s\n\n' "$missing" >&2
  usage
fi

# strip leading/trailing slashes from the prefix
S3_PREFIX="${S3_PREFIX#/}"; S3_PREFIX="${S3_PREFIX%/}"
S3_URL="s3://$S3_BUCKET${S3_PREFIX:+/$S3_PREFIX}"

if [ -z "$MIGRATE_BIN" ]; then
  if [ -x ./migrate-rails ]; then
    MIGRATE_BIN=./migrate-rails
  elif command -v migrate-rails >/dev/null 2>&1; then
    MIGRATE_BIN=$(command -v migrate-rails)
  else
    die "migrate-rails binary not found; build it with: go build -o migrate-rails ./cmd/migrate-rails (or set MIGRATE_BIN)"
  fi
fi

GO_DB="$DATA_DIR/rables.db"

step "配置确认"
info "RAILS_DB     = $RAILS_DB"
info "S3 source    = $S3_URL"
info "DATA_DIR     = $DATA_DIR"
info "MIGRATE_BIN  = $MIGRATE_BIN"
info "S3_DUMP_DIR  = ${S3_DUMP_DIR:-<mktemp, 运行后打印>}"
info "REPORT       = $REPORT"

# ---- preflight checks -----------------------------------------------------

step "环境预检"
for cmd in sqlite3 aws; do
  command -v "$cmd" >/dev/null 2>&1 || die "'$cmd' 不在 PATH 中，请先安装"
done
[ -x "$MIGRATE_BIN" ] || die "MIGRATE_BIN 不可执行: $MIGRATE_BIN"
[ -f "$RAILS_DB" ] || die "RAILS_DB 不存在: $RAILS_DB"
[ -s "$RAILS_DB" ] || die "RAILS_DB 是 0 字节空文件: $RAILS_DB"
blob_count=$(sqlite3 -readonly "$RAILS_DB" "SELECT COUNT(*) FROM active_storage_blobs;" 2>/dev/null) \
  || die "$RAILS_DB 不像 Rails rables 数据库（查询 active_storage_blobs 失败）。若 ${RAILS_DB}-wal 存在但缺少对应的 -shm，只读模式同样打不开 —— 请把 -wal 与 -shm 一并放齐（见下文快照步骤的说明）后重试"
aws s3 ls "$S3_URL/" >/dev/null 2>&1 \
  || die "无法访问 $S3_URL —— 检查 bucket 名、S3_PREFIX、AWS 凭证和 region"
info "sqlite3 / aws / migrate-rails 就绪，S3 bucket 可访问"

# ---- plan -----------------------------------------------------------------

step "迁移计划"
articles=$(sqlite3 -readonly "$RAILS_DB" "SELECT COUNT(*) FROM articles;")
pages=$(sqlite3 -readonly "$RAILS_DB" "SELECT COUNT(*) FROM pages;")
comments=$(sqlite3 -readonly "$RAILS_DB" "SELECT COUNT(*) FROM comments;")
bytes=$(sqlite3 -readonly "$RAILS_DB" "SELECT COALESCE(SUM(byte_size),0) FROM active_storage_blobs;")
size_mb=$(awk -v b="$bytes" 'BEGIN { printf "%.1f", b/1048576 }')
info "Rails 库内容: ${articles} 文章 / ${pages} 页面 / ${comments} 评论 / ${blob_count} 个文件 (${size_mb} MB)"
info "将执行:"
info "1) 快照 Rails 数据库（sqlite3 .backup，不停机、只读）"
info "2) aws s3 sync $S3_URL → 本地缓存目录"
info "3) 按 ActiveStorage disk 布局摆放到 $DATA_DIR/files/xx/yy/<key>"
info "4) migrate-rails --verify-files 迁移全部数据表并重写正文文件引用"
if [ -f "$GO_DB" ]; then
  warn "$GO_DB 已存在 —— 后面会询问处理方式（delete=备份后清空重来 / keep=增量追平）"
  warn "注意: 已有 settings/users 行会阻止旧库对应行迁入（migrate-rails 会跳过并在报告里标注）"
fi

if [ "$ASSUME_YES" != "yes" ]; then
  if [ -t 0 ]; then
    printf '\n确认执行? [y/N] '
    read -r ans
    [ "$ans" = "y" ] || [ "$ans" = "Y" ] || die "已取消"
  else
    die "非交互环境请显式设置 ASSUME_YES=yes"
  fi
fi

# ---- existing Go DB handling ----------------------------------------------

mkdir -p "$DATA_DIR" 2>/dev/null || die "无法创建 ${DATA_DIR}（权限不足？试试 sudo 运行）"

if [ -f "$GO_DB" ]; then
  action="$EXISTING_DB"
  if [ -z "$action" ]; then
    if [ -t 0 ]; then
      printf '\n%s 已存在。选择: [d]elete=备份后删除重建 / [k]eep=增量追平 / [a]bort=取消: ' "$GO_DB"
      read -r ans
      case "$ans" in
        d|D) action=delete ;;
        k|K) action=keep ;;
        *)   die "已取消" ;;
      esac
    else
      die "$GO_DB 已存在；请显式设置 EXISTING_DB=delete 或 EXISTING_DB=keep"
    fi
  fi
  case "$action" in
    delete)
      # pgrep -x matches the exact process name only. The systemd unit and the
      # Docker entrypoint install the binary as rables-server, but a dev build
      # from the repo root is just "server" — check both.
      if pgrep -x rables-server >/dev/null 2>&1 || pgrep -x server >/dev/null 2>&1; then
        die "检测到 rables-server/server 正在运行。先停止（systemctl stop rables 或 docker stop <容器>）再删除其数据库"
      fi
      backup="$GO_DB.pre-migration-$TS"
      step "备份现有 Go 数据库 → $backup"
      sqlite3 "$GO_DB" ".backup '$backup'"
      rm -f "$GO_DB" "$GO_DB-wal" "$GO_DB-shm"
      info "已备份并清空，将迁入全新数据库"
      ;;
    keep)
      info "保留现有数据库，执行增量追平"
      warn "若 Go 版已跑过 setup，旧的 settings/users 行将被跳过（见迁移报告 note）"
      ;;
    *)
      die "EXISTING_DB 只能是 delete 或 keep（当前: ${action}）"
      ;;
  esac
fi

# ---- 1. snapshot the Rails DB ----------------------------------------------

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"; find "$DATA_DIR/files" -type f -name "*.tmp.$$" -delete 2>/dev/null || true' EXIT
SNAPSHOT="$WORK/rails-snapshot.sqlite3"

step "1/4 快照 Rails 数据库"
if ! sqlite3 -readonly "$RAILS_DB" ".backup '$SNAPSHOT'"; then
  if [ -f "$RAILS_DB-wal" ] && [ ! -f "$RAILS_DB-shm" ]; then
    die "快照失败：$RAILS_DB 带有残留 WAL 但缺少 -shm（Rails 非正常退出后只拷贝了部分文件？只读模式无法恢复热 WAL）。请把 ${RAILS_DB}-wal 与 ${RAILS_DB}-shm 一并放齐，或先正常启停一次 Rails 再重试"
  fi
  die "快照 $RAILS_DB 失败"
fi
info "快照完成（Rails 无需停机，WAL 安全）"

# ---- 2. download S3 blobs ---------------------------------------------------

if [ -z "$S3_DUMP_DIR" ]; then
  S3_DUMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/rables-s3dump.XXXXXX")
fi
mkdir -p "$S3_DUMP_DIR"

step "2/4 下载 S3 文件 → $S3_DUMP_DIR"
aws s3 sync "$S3_URL" "$S3_DUMP_DIR" --only-show-errors
dump_count=$(find "$S3_DUMP_DIR" -type f | wc -l | tr -d ' ')
info "下载完成，共 $dump_count 个对象"

# ---- 3. lay blobs out in ActiveStorage disk layout --------------------------

step "3/4 摆放文件到 $DATA_DIR/files/xx/yy/<key>"
sqlite3 "$SNAPSHOT" "SELECT key FROM active_storage_blobs ORDER BY id;" > "$WORK/keys.txt"
copied=0; skipped=0; missing_count=0
: > "$WORK/missing.txt"
while IFS= read -r k; do
  [ -n "$k" ] || continue
  # ActiveStorage keys are base58 alphanumerics; reject anything else before
  # it is interpolated into a filesystem path.
  if [ "${#k}" -lt 4 ] || [ -n "${k//[a-zA-Z0-9]/}" ]; then
    echo "$k (invalid key)" >> "$WORK/missing.txt"
    missing_count=$((missing_count + 1))
    continue
  fi
  dst="$DATA_DIR/files/${k:0:2}/${k:2:2}/$k"
  if [ -f "$dst" ]; then
    skipped=$((skipped + 1))
    continue
  fi
  src="$S3_DUMP_DIR/$k"
  if [ -f "$src" ]; then
    mkdir -p "${dst%/*}"
    # Copy to a temp name in the same directory, then mv: the rename is
    # atomic on the same filesystem, so an interrupted run never leaves a
    # truncated blob behind that --verify-files (existence check only)
    # would silently accept. Leftover temp files are removed by the EXIT trap.
    # (An if/else, not cp && mv: a failing cp inside an && list does not
    # trigger set -e.)
    tmp="$dst.tmp.$$"
    if cp "$src" "$tmp"; then
      mv "$tmp" "$dst"
      copied=$((copied + 1))
    else
      die "拷贝 $k 失败: $src → $dst"
    fi
  else
    echo "$k" >> "$WORK/missing.txt"
    missing_count=$((missing_count + 1))
  fi
done < "$WORK/keys.txt"
# Orphans are bucket objects no database key references; compute the set
# difference instead of arithmetic so a reused dump dir stays accurate.
(cd "$S3_DUMP_DIR" && find . -type f | sed 's|^\./||' | LC_ALL=C sort) > "$WORK/dump_files.txt"
LC_ALL=C sort "$WORK/keys.txt" > "$WORK/keys_sorted.txt"
orphans=$(LC_ALL=C comm -23 "$WORK/dump_files.txt" "$WORK/keys_sorted.txt" | wc -l | tr -d ' ')
info "新摆放 $copied 个 / 已存在跳过 $skipped 个 / S3 中缺失 $missing_count 个"
[ "$orphans" -le 0 ] || info "另有 $orphans 个 bucket 对象未被数据库引用，留在缓存目录未处理"
if [ "$missing_count" -gt 0 ]; then
  warn "以下 key 在 S3 中找不到（完整列表见最终报告）:"
  head -10 "$WORK/missing.txt" | sed 's/^/    /' >&2
fi

# ---- 4. run the database migration ------------------------------------------

step "4/4 运行 migrate-rails（报告写入 ${REPORT}）"
set +e
"$MIGRATE_BIN" -old "$SNAPSHOT" -data "$DATA_DIR" --verify-files 2>&1 | tee "$REPORT"
rc=${PIPESTATUS[0]}
set -e

# ---- ownership fixup (root run only) ----------------------------------------

if [ -z "$CHOWN_USER" ] && [ "$(id -u)" -eq 0 ] && id rables >/dev/null 2>&1; then
  CHOWN_USER=rables
fi
if [ -n "$CHOWN_USER" ] && [ "$CHOWN_USER" != "none" ]; then
  # numeric uid (e.g. 65532 for the distroless container user) needs no lookup
  case "$CHOWN_USER" in
    *[!0-9]*)
      id "$CHOWN_USER" >/dev/null 2>&1 || die "CHOWN_USER 用户不存在: ${CHOWN_USER}"
      # the user's primary group — a same-named group is not guaranteed to exist
      chown_group=$(id -gn "$CHOWN_USER")
      ;;
    *) chown_group="$CHOWN_USER" ;;
  esac
  if chown -R "$CHOWN_USER":"$chown_group" "$DATA_DIR"; then
    info "已将 $DATA_DIR 属主改为 $CHOWN_USER:$chown_group"
  else
    warn "chown 失败（需要 root 运行？）—— 迁移本身已完成，请手动执行: sudo chown -R $CHOWN_USER:$chown_group $DATA_DIR"
  fi
fi

# ---- summary -----------------------------------------------------------------

step "完成总结"
info "数据库快照: 已创建并在结束后清理（临时目录）"
info "S3 下载: $dump_count 个对象 → $S3_DUMP_DIR"
info "文件摆放: 新增 ${copied} / 跳过 ${skipped} / 缺失 ${missing_count}（目标 ${DATA_DIR}/files/）"
[ -f "$WORK/missing.txt" ] && [ "$missing_count" -gt 0 ] && \
  sed 's/^/    missing: /' "$WORK/missing.txt" >> "$REPORT" || true
info "数据库迁移: 报告在 $REPORT"
if [ "$rc" -eq 0 ] && [ "$missing_count" -eq 0 ]; then
  info "结果: OK —— 行数对账一致、文件全部就位"
else
  warn "结果: 需要人工检查（migrate 退出码 ${rc}，缺失文件 ${missing_count}）—— 详见报告"
fi

cat <<EOF

下一步:
  1. 检查 $REPORT 中的 "kept references"（无法自动重写、需手工修正的正文引用）
  2. 若 Rails 还在运行并产生新内容: 切换前用 EXISTING_DB=keep S3_DUMP_DIR=$S3_DUMP_DIR 重跑本脚本追平增量
  3. 切换: 反向代理指向 Go 服务端口，停掉 Rails（先保留几天作回滚）
  4. 验证网站无误后: 删除缓存目录 ${S3_DUMP_DIR}；S3 bucket 建议保留几周再删
EOF

exit "$rc"
