#!/usr/bin/env bash
# Nightly Postgres dump + SeaweedFS volume tarball; optional off-VM sync (ADR-031).
# Install via cron — see backup.cron.example and docs/runbooks/demo-environment.md.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

ENV_FILE="${ENV_FILE:-deploy/demo/.env.demo}"
if [[ -f "$ENV_FILE" ]]; then
	set -a
	# shellcheck source=/dev/null
	source "$ENV_FILE"
	set +a
fi

BACKUP_DIR="${BACKUP_DIR:-/var/backups/yc-demo}"
BACKUP_KEEP_DAYS="${BACKUP_KEEP_DAYS:-14}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
DEST="${BACKUP_DIR}/${STAMP}"
mkdir -p "$DEST"

echo "==> pg_dumpall → ${DEST}/pg.dump"
docker exec yc-postgres pg_dumpall -U postgres | gzip -c >"${DEST}/pg.dump.gz"

echo "==> seaweedfs data → ${DEST}/s3data.tar.gz"
docker run --rm \
	--volumes-from yc-seaweedfs \
	-v "${DEST}:/backup" \
	alpine:3.20 \
	tar czf /backup/s3data.tar.gz -C /data .

echo "${STAMP}" >"${BACKUP_DIR}/latest"
echo "==> wrote ${DEST}"

if [[ -n "${BACKUP_REMOTE:-}" ]]; then
	echo "==> sync to ${BACKUP_REMOTE}"
	# Prefer rclone when configured; fall back to rsync URI.
	if command -v rclone >/dev/null 2>&1 && [[ "$BACKUP_REMOTE" == *:* ]]; then
		rclone copy "$DEST" "${BACKUP_REMOTE}/${STAMP}"
		rclone copy "${BACKUP_DIR}/latest" "${BACKUP_REMOTE}/"
	else
		rsync -a "$DEST" "${BACKUP_REMOTE}/"
		rsync -a "${BACKUP_DIR}/latest" "${BACKUP_REMOTE}/"
	fi
fi

echo "==> prune older than ${BACKUP_KEEP_DAYS} days"
find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -type d -mtime "+${BACKUP_KEEP_DAYS}" -exec rm -rf {} +

echo "backup complete"
