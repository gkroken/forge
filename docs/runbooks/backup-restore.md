# Backup & restore

## What needs backing up

| Store | Eval (filesystem) | Production |
|-------|-------------------|------------|
| Blob store (artifacts) | `data/blobs/` | S3 bucket |
| Meta store (config, packuments, cache entries) | `data/meta/` | Postgres database |

Both must be backed up together. A meta backup without a corresponding blob backup (or vice versa) will produce an inconsistent restore.

## Filesystem backup (eval mode)

```bash
# Stop forge first to avoid partial writes during backup
kill -TERM $(pgrep forge)

# Archive both stores
tar -czf forge-backup-$(date +%Y%m%d-%H%M%S).tar.gz data/

# Restart
./forge -addr :8080 -data ./data &
```

For a live backup (without stopping), use a filesystem snapshot if your storage supports it (LVM, ZFS, cloud PVC snapshots).

## S3 backup (production)

Use S3 versioning + lifecycle rules on the bucket. For a point-in-time backup:

```bash
aws s3 sync s3://forge-artifacts s3://forge-artifacts-backup-$(date +%Y%m%d) \
  --source-region us-east-1
```

Or enable S3 Cross-Region Replication for continuous backup.

## Postgres backup (production)

```bash
pg_dump $POSTGRES_DSN | gzip > forge-meta-$(date +%Y%m%d-%H%M%S).sql.gz
```

Schedule this via cron or your managed Postgres provider's automated backup feature. Retain at least 7 daily backups.

## Restore procedure

### Filesystem (eval)

```bash
# Stop forge
kill -TERM $(pgrep forge)

# Remove current data
rm -rf data/

# Extract backup
tar -xzf forge-backup-20240101-120000.tar.gz

# Restart
./forge -addr :8080 -data ./data &

# Verify
curl -sf http://localhost:8080/healthz
```

### Production (S3 + Postgres)

1. **Restore Postgres** to the target point in time using your provider's restore tooling or `psql < backup.sql`.
2. **Identify the S3 snapshot** that matches the Postgres restore point. If using versioning, restore the bucket to that version timestamp.
3. **Update connection strings** if the restored database is at a new endpoint.
4. **Start forge** and verify with `/healthz` + a test artifact download.

## What is safe to lose

- **Proxy cache entries** — these are regenerated on the next upstream fetch. Losing them causes cache misses but not data loss.
- **npm packuments and Helm index.yaml** — regenerated from the underlying per-version records and blob store on next request.

**Not safe to lose:**
- Blob store (the actual artifact bytes)
- Meta records for hosted artifacts (version records, CRAN DESCRIPTION data)
- Token and repository configuration

## Backup verification

Periodically restore to a staging environment and confirm:

```bash
curl -sf http://staging:8080/healthz
# Download a known artifact and verify its checksum
curl -sf http://staging:8080/repository/maven-hosted/com/example/lib/1.0.0/lib-1.0.0.jar \
  | sha256sum
```
