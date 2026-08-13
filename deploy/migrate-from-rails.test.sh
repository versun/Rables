#!/usr/bin/env bash
# migrate-from-rails.test.sh — regression tests for migrate-from-rails.sh.
#
# Runs the script against stubbed sqlite3 / aws / migrate-rails / pgrep on a
# sandboxed PATH; no real database, S3 bucket, or server process is touched.
#
# Usage: bash deploy/migrate-from-rails.test.sh

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$SCRIPT_DIR/migrate-from-rails.sh"

pass=0; fail=0
ok()  { printf 'ok   - %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf 'FAIL - %s\n' "$1" >&2; fail=$((fail + 1)); }

# make_sandbox <name> — creates $T with stub bin/, fixture db, and control dir.
make_sandbox() {
  T=$(mktemp -d "${TMPDIR:-/tmp}/rables-migrate-test.XXXXXX")
  mkdir -p "$T/bin" "$T/data" "$T/s3"
  echo "not a real sqlite file" > "$T/rails.sqlite3"

  # Stub sqlite3: answers the exact queries the script issues; behavior is
  # driven by control files in $STUB_DIR (fail_readonly, keys).
  cat > "$T/bin/sqlite3" <<'EOF'
#!/usr/bin/env bash
[ "${1:-}" = "-readonly" ] && shift
db=$1; shift
sql=${1:-}
case "$sql" in
  "SELECT COUNT(*) FROM active_storage_blobs;")
    [ -f "$STUB_DIR/fail_readonly" ] && exit 1
    wc -l < "$STUB_DIR/keys" | tr -d ' ' ;;
  "SELECT key FROM active_storage_blobs ORDER BY id;")
    cat "$STUB_DIR/keys" ;;
  "SELECT COALESCE(SUM(byte_size),0) FROM active_storage_blobs;")
    echo 2048 ;;
  "SELECT COUNT(*) FROM "*)
    echo 1 ;;
  .backup*)
    target=$(printf '%s' "$sql" | sed "s/^\.backup '//; s/'\$//")
    /bin/cp "$db" "$target" ;;
  *)
    echo "stub sqlite3: unexpected query: $sql" >&2; exit 1 ;;
esac
EOF

  # Stub aws: s3 ls succeeds; s3 sync copies fixture objects from $STUB_DIR/s3.
  cat > "$T/bin/aws" <<'EOF'
#!/usr/bin/env bash
case "${2:-}" in
  ls)   exit 0 ;;
  sync) mkdir -p "$4"; /bin/cp -R "$STUB_DIR/s3/." "$4" ;;
  *)    echo "stub aws: unexpected args: $*" >&2; exit 1 ;;
esac
EOF

  # Stub migrate-rails: succeed without doing anything.
  cat > "$T/bin/migrate-rails" <<'EOF'
#!/usr/bin/env bash
echo "stub migrate-rails $*"
exit 0
EOF

  # Stub pgrep: succeeds when the queried name is listed in $STUB_DIR/running.
  cat > "$T/bin/pgrep" <<'EOF'
#!/usr/bin/env bash
name=${2:-${1:-}}
[ -f "$STUB_DIR/running" ] && grep -qx "$name" "$STUB_DIR/running"
EOF

  chmod +x "$T/bin/"*
  printf 'aa11bb22cc33\ndd44ee55ff66\n' > "$T/keys"
  echo "content-of-aa11bb22cc33" > "$T/s3/aa11bb22cc33"
  echo "content-of-dd44ee55ff66" > "$T/s3/dd44ee55ff66"
}

# run_script [VAR=value ...] — runs the script in $T, captures output to $T/out.
run_script() {
  env -i PATH="$T/bin:/usr/bin:/bin" HOME="${HOME:-/tmp}" STUB_DIR="$T" \
    RAILS_DB="$T/rails.sqlite3" S3_BUCKET=test-bucket \
    DATA_DIR="$T/data" S3_DUMP_DIR="$T/dump" \
    MIGRATE_BIN="$T/bin/migrate-rails" \
    ASSUME_YES=yes CHOWN_USER=none REPORT="$T/report.txt" \
    "$@" bash "$SCRIPT" > "$T/out" 2>&1
}

# --- test 1: blobs land atomically, no temp files left, re-run skips ---------

make_sandbox happy
if run_script; then ok "happy path exits 0"; else bad "happy path exits 0"; fi

dst1="$T/data/files/aa/11/aa11bb22cc33"
dst2="$T/data/files/dd/44/dd44ee55ff66"
if [ -f "$dst1" ] && [ "$(cat "$dst1")" = "content-of-aa11bb22cc33" ] &&
   [ -f "$dst2" ] && [ "$(cat "$dst2")" = "content-of-dd44ee55ff66" ]; then
  ok "blobs laid out with correct content"
else
  bad "blobs laid out with correct content"
fi

if [ -z "$(find "$T/data" -name '*.tmp.*' -print -quit)" ]; then
  ok "no *.tmp.* files left behind"
else
  bad "no *.tmp.* files left behind"
fi

if run_script && grep -q "已存在跳过 2 个" "$T/out"; then
  ok "catch-up re-run skips existing blobs"
else
  bad "catch-up re-run skips existing blobs"
fi
rm -rf "$T"

# --- test 2: interrupted copy leaves no truncated/partial blob ---------------

make_sandbox interrupted
# Stub cp to simulate a copy cut short: writes a partial temp file, then fails.
cat > "$T/bin/cp" <<'EOF'
#!/usr/bin/env bash
printf 'partial' > "$2"
exit 1
EOF
chmod +x "$T/bin/cp"

if run_script; then bad "interrupted copy aborts the run"; else ok "interrupted copy aborts the run"; fi
if [ ! -f "$T/data/files/aa/11/aa11bb22cc33" ]; then
  ok "no truncated blob at the destination"
else
  bad "no truncated blob at the destination"
fi
if [ -z "$(find "$T/data" -name '*.tmp.*' -print -quit)" ]; then
  ok "leftover temp copy cleaned up on exit"
else
  bad "leftover temp copy cleaned up on exit"
fi
rm -rf "$T"

# --- test 3: preflight failure mentions the hot-WAL / missing -shm case ------

make_sandbox wal
: > "$T/fail_readonly"
if run_script; then bad "preflight fails when rails db is unreadable"; else ok "preflight fails when rails db is unreadable"; fi
if grep -q "不像 Rails rables 数据库" "$T/out" && grep -q -- "-shm" "$T/out"; then
  ok "preflight error points at missing -shm / hot WAL"
else
  bad "preflight error points at missing -shm / hot WAL"
fi
rm -rf "$T"

# --- test 4: delete is blocked when a dev-built "server" process runs --------

make_sandbox guard
echo x > "$T/data/rables.db"
echo server > "$T/running"
if run_script EXISTING_DB=delete; then
  bad "delete blocked while dev 'server' process runs"
else
  ok "delete blocked while dev 'server' process runs"
fi
if grep -q "正在运行" "$T/out" && [ -f "$T/data/rables.db" ]; then
  ok "existing Go DB left untouched"
else
  bad "existing Go DB left untouched"
fi
rm -rf "$T"

# --- test 5: delete proceeds when no server process runs ---------------------

make_sandbox delete_ok
echo x > "$T/data/rables.db"
if run_script EXISTING_DB=delete; then ok "delete proceeds when nothing runs"; else bad "delete proceeds when nothing runs"; fi
if [ ! -f "$T/data/rables.db" ] && compgen -G "$T/data/rables.db.pre-migration-*" > /dev/null; then
  ok "existing Go DB backed up and removed"
else
  bad "existing Go DB backed up and removed"
fi
rm -rf "$T"

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
