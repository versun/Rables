#!/usr/bin/env bash
# Online backup of a running Rables instance.
#
#   1. `sqlite3 .backup` — consistent online copy of rables.db (WAL-safe,
#      the server keeps serving while this runs).
#   2. tar of DATA_DIR/files/ — uploaded media (exports/ is regenerable and
#      therefore skipped).
#
# Usage:
#   ./backup.sh                      # uses the defaults below
#   DATA_DIR=/srv/rables BACKUP_DIR=/backups KEEP=14 ./backup.sh
#
# Optional off-site copy (uncomment):
#   rsync -a --delete "$BACKUP_DIR/" user@backup-host:/srv/backups/rables/
set -euo pipefail

DATA_DIR="${DATA_DIR:-/var/lib/rables}"
BACKUP_DIR="${BACKUP_DIR:-/var/backups/rables}"
KEEP="${KEEP:-14}"

ts="$(date -u +%Y%m%dT%H%M%SZ)"
dest="$BACKUP_DIR/$ts"
mkdir -p "$dest"

sqlite3 "$DATA_DIR/rables.db" ".backup '$dest/rables.db'"

if [ -d "$DATA_DIR/files" ]; then
  tar -C "$DATA_DIR" -czf "$dest/files.tar.gz" files
fi

# Retention: keep only the newest KEEP snapshots.
ls -1dt "$BACKUP_DIR"/*/ | tail -n +"$((KEEP + 1))" | xargs -r rm -rf

echo "backup complete: $dest"
