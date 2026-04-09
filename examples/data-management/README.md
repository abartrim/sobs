# SOBS – Data Management, Backup, Retention & S3

This directory provides reference configuration and examples for SOBS data
management features:

- **Backup to S3** – automated full and incremental backups of embedded chDB
  data to an S3-compatible bucket.
- **Data retention TTLs** – automatic expiry of logs, traces, metric sessions,
  and other signal types after a configurable number of days/hours.
- **Restore** – one-click restore from a named backup.

---

## Quick-start: enable S3 backups

### 1. Configure S3 credentials in SOBS Settings → Data Management

Open `http://localhost:44317/settings/data-management` and fill in:

| Field | Value |
|---|---|
| S3 Bucket | `my-sobs-backups` |
| S3 Region | `us-east-1` |
| Access Key ID | `AKIAIOSFODNN7EXAMPLE` |
| Secret Access Key | `wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY` *(use a placeholder or secret manager in production)* |
| Path prefix | `sobs/` *(optional)* |
| Encrypt backup | ✓ *(recommended; set a strong backup encryption password)* |

Alternatively, use the API (see below).

### 2. Schedule backups

Set cron-style schedules in the same settings page, e.g.:
- **Full backup**: `0 2 * * *` (daily at 02:00)
- **Incremental backup**: `0 */6 * * *` (every 6 hours)

---

## API reference

### List available backups

```bash
curl -s http://localhost:44317/api/data-management/backup/list \
  -H "X-API-Key: $SOBS_API_KEY"
```

### Run a backup manually

```bash
# Full backup
curl -s -X POST http://localhost:44317/api/data-management/backup/run \
  -H "Content-Type: application/json" \
  -H "X-API-Key: $SOBS_API_KEY" \
  -d '{"type": "full"}'

# Incremental backup
curl -s -X POST http://localhost:44317/api/data-management/backup/run \
  -H "Content-Type: application/json" \
  -H "X-API-Key: $SOBS_API_KEY" \
  -d '{"type": "incremental"}'
```

### Restore from a backup

```bash
# Replace <backup-name> with a name returned by the list endpoint.
curl -s -X POST http://localhost:44317/api/data-management/restore \
  -H "Content-Type: application/json" \
  -H "X-API-Key: $SOBS_API_KEY" \
  -d '{"backup_name": "<backup-name>"}'
```

---

## Retention TTL settings

Retention TTLs are configured in **Settings → Data Management** and control how
long SOBS keeps each signal type.

| Signal | Setting key | Default |
|--------|-------------|---------|
| Logs | `data_management.ttl_logs_days` | *(no TTL)* |
| Traces | `data_management.ttl_traces_days` | *(no TTL)* |
| RUM sessions | `data_management.ttl_sessions_days` | *(no TTL)* |
| Raw metrics | `SOBS_RAW_METRICS_TTL_HOURS` env var | 48 hours |
| Pinned metrics | `SOBS_PINNED_METRICS_TTL_DAYS` env var | 14 days |

> **Tip:** Enable **TTL–backup coupling** in settings to prevent SOBS from
> deleting data that has not yet been backed up.

### Set retention via env vars (raw metrics only)

```yaml
# docker-compose.yml excerpt
environment:
  - SOBS_RAW_METRICS_TTL_HOURS=72      # keep raw metric data points for 72 h
  - SOBS_PINNED_METRICS_TTL_DAYS=30    # keep pinned/retention-window data for 30 days
```

---

## S3-compatible storage (MinIO example)

The docker-compose below runs SOBS together with a local MinIO instance for
development and testing.

```yaml
version: "3.9"

services:
  sobs:
    image: ghcr.io/abartrim/sobs:latest
    ports:
      - "44317:4317"
    volumes:
      - sobs_data:/data
    environment:
      - PORT=4317
      - SOBS_SECRET_KEY=change-me
      - SOBS_RAW_METRICS_TTL_HOURS=48
      - SOBS_PINNED_METRICS_TTL_DAYS=14
    restart: unless-stopped

  minio:
    image: minio/minio:latest
    command: server /data --console-address ":9001"
    ports:
      - "9000:9000"
      - "9001:9001"
    volumes:
      - minio_data:/data
    environment:
      - MINIO_ROOT_USER=minioadmin
      - MINIO_ROOT_PASSWORD=minioadmin  # change in production

volumes:
  sobs_data:
  minio_data:
```

After starting the stack, open `http://localhost:9001` (MinIO console) to
create a bucket named `sobs-backups`, then configure SOBS with:

| Field | Value |
|---|---|
| S3 Bucket | `sobs-backups` |
| S3 Region | *(leave empty for MinIO)* |
| Access Key ID | `minioadmin` |
| Secret Access Key | `minioadmin` |
| Endpoint override | `http://minio:9000` *(set in S3 URL if supported, or via custom endpoint)* |

---

## Security notes

- **Never commit real S3 credentials.** Use Docker secrets, Kubernetes secrets,
  or a secrets manager in production.
- Enable **backup encryption** with a strong password for S3 backups — the
  password is stored in SOBS settings and can itself be protected with
  `SOBS_SETTINGS_ENCRYPTION_KEY`.
- Restrict the S3 IAM policy to `PutObject`, `GetObject`, `ListBucket` on the
  backup bucket only.
