# pg18backrest

`ghcr.io/jptrs93/pg18backrest` is PostgreSQL 18.4 on Debian Bookworm with optional [pgBackRest](https://pgbackrest.org/) backups to one S3-compatible object store. A small Go supervisor is PID 1: it owns PostgreSQL startup, PostgreSQL reconciliation, backup scheduling, and health reporting.

It preserves the normal `postgres` image initialization interface. When `PGBACKREST_ENABLED=false` (the default), it behaves as PostgreSQL without backup configuration.

## Run

Copy [`.env.example`](.env.example) to `.env`, copy [`config.example.yaml`](config.example.yaml) to `config.yaml`, set real credentials and desired PostgreSQL state, then start the supplied Compose file:

```bash
docker compose --env-file .env -f compose.example.yaml up -d
```

Mount `/var/lib/postgresql`, not the pre-PostgreSQL-18 `/var/lib/postgresql/data` path. PostgreSQL data is stored at `/var/lib/postgresql/18/docker`; pgBackRest's async spool and health state share the same persistent volume.

The S3 bucket must already exist. The stanza separates clusters within the bucket. With `PGBACKREST_S3_URI_STYLE=path` and `repo1-path=/`, pgBackRest writes to:

```text
<bucket>/archive/<stanza>/...
<bucket>/backup/<stanza>/...
```

## Supervisor Configuration

The supervisor reads strict YAML from `POSTGRES_SUPERVISOR_CONFIG`, defaulting to `/etc/postgres-supervisor/config.yaml`. Unknown YAML fields are rejected. The file is read at container start; restart the container after changing `settings` or `hba`.

```yaml
settings:
  max_connections: 100
  shared_buffers: 256MB
hba:
  - host all all 10.0.0.0/8 scram-sha-256
roles:
  - name:
      value: app_user
      # env: APP_USER
    permissions:
      - database: radkit
        # schema: public # all non-system schemas when omitted
        grants:
          - CREATE
          - USAGE
databases:
  - name: radkit
    owner: app_user
    schemas:
      - staging
    extensions:
      - pgcrypto
```

`settings` is a map of PostgreSQL setting names to scalar YAML values. Strings are safely quoted in the generated `postgresql.conf`; numeric and boolean values are emitted as PostgreSQL scalars. `data_directory`, `hba_file`, `ident_file`, and `config_file` are reserved because the supervisor manages them.

`hba` contains complete `pg_hba.conf` records. The supervisor always prepends `local all all trust` so its local `pgx` reconciliation connection is available inside the container. If `hba` is omitted, the image additionally allows remote `scram-sha-256` password authentication for all users.

Each `roles[].name` has exactly one of `value` or `env`. New roles are created with `LOGIN`; existing roles are not altered. Each permission grants only the listed `CREATE` and/or `USAGE` schema privileges. A missing `schema` applies the grant to all current non-system schemas in the specified database.

The reconciler runs with `github.com/jackc/pgx/v5` after PostgreSQL is ready. It creates missing roles, databases, schemas, extensions, and explicit grants. It does not delete or alter databases, schemas, roles, extensions, or grants that were removed from YAML. The default local reconciliation user is `POSTGRES_USER`, falling back to `postgres`; set `POSTGRES_SUPERVISOR_ADMIN_USER` or `POSTGRES_SUPERVISOR_ADMIN_URL` when needed.

## Required Backup Variables

Set these whenever `PGBACKREST_ENABLED=true` or `PGBACKREST_RESTORE_ENABLED=true`.

| Variable | Description |
| --- | --- |
| `PGBACKREST_STANZA` | Backup namespace for this PostgreSQL cluster. |
| `PGBACKREST_S3_ENDPOINT` | S3-compatible endpoint, such as `https://s3.example.internal`. |
| `PGBACKREST_S3_BUCKET` | Existing bucket containing the repository. |
| `PGBACKREST_S3_KEY` | S3 access key. |
| `PGBACKREST_S3_KEY_SECRET` | S3 secret access key. |

`PGBACKREST_S3_KEY_FILE`, `PGBACKREST_S3_KEY_SECRET_FILE`, and `PGBACKREST_REPO_CIPHER_PASS_FILE` load values from files, which is suitable for Docker secrets. Each `_FILE` variable is mutually exclusive with its equivalent environment variable.

## Optional Backup Variables

| Variable | Default | Description |
| --- | --- | --- |
| `PGBACKREST_S3_REGION` | `us-east-1` | S3 signing region. |
| `PGBACKREST_S3_URI_STYLE` | `path` | `path` or `host` addressing. |
| `PGBACKREST_S3_VERIFY_TLS` | `true` | Set `false` only for a trusted self-signed endpoint. |
| `PGBACKREST_REPO_CIPHER_PASS` | unset | Enables client-side AES-256-CBC repository encryption. Store this value securely and never change it for an existing repository. |
| `PGBACKREST_RETENTION_FULL` | `2` | Number of completed full backup sets retained. pgBackRest needs space for retention plus one additional full backup before expiry. |
| `PGBACKREST_RETENTION_ARCHIVE` | `2` | Archive retention count, not a number of days. It retains WAL required by retained backup sets. |
| `PGBACKREST_PROCESS_MAX` | `4` | Concurrent pgBackRest transfer/compression processes. |
| `PGBACKREST_ARCHIVE_PUSH_QUEUE_MAX` | `1GiB` | Maximum local async WAL queue. Exceeding it drops queued WAL so PostgreSQL stays available, but creates a PITR gap. Monitor it. |
| `PGBACKREST_ARCHIVE_TIMEOUT` | `60` | Seconds a backup waits for required WAL to reach S3. |
| `PGBACKREST_INITIAL_BACKUP` | `true` | Takes a full backup after the stanza's first successful check. |
| `PGBACKREST_FULL_SCHEDULE` | `0 2 * * 0` | Five-field cron schedule for full backups. |
| `PGBACKREST_DIFF_SCHEDULE` | `0 2 * * 1-6` | Five-field cron schedule for differential backups. |
| `PGBACKREST_CHECK_SCHEDULE` | `*/5 * * * *` | Five-field cron schedule for `pgbackrest check`. |
| `TZ` | `UTC` | Time zone used for backup schedules. |

The Go scheduler accepts standard five-field cron syntax with ranges, steps, lists, and wildcards. All backup commands run as the `postgres` operating-system user.

## Startup And Health

When backup is enabled, PostgreSQL starts immediately. The image then creates and checks the stanza in the background. A temporary S3 outage or invalid remote credentials does not stop PostgreSQL, but the Docker healthcheck stays unhealthy until the repository check succeeds.

The healthcheck verifies PostgreSQL readiness, successful reconciliation, and the latest pgBackRest maintenance result. It deliberately does not run `pgbackrest check` itself because that command forces a WAL switch. The scheduled check detects repository availability at the configured interval. The supervisor also serves the same result at `GET /healthz` on port `8081`, configurable with `POSTGRES_SUPERVISOR_HEALTH_ADDR`.

Inspect backup status from the running container:

```bash
docker exec -u postgres <container> pgbackrest --stanza=<stanza> info
```

Force a backup:

```bash
docker exec -u postgres <container> pgbackrest --stanza=<stanza> --type=full backup
docker exec -u postgres <container> pgbackrest --stanza=<stanza> --type=diff backup
```

`pg_isready` only proves that PostgreSQL accepts connections. It does not prove that WAL is reaching the repository. Use `pgbackrest info` and restore tests as operational checks.

## Restore

Restores are explicit. Set `PGBACKREST_RESTORE_ENABLED=true` and use an empty PostgreSQL volume. Restore mode refuses populated `PGDATA` rather than overwriting a cluster.

For an isolated recovery test, disable future archiving:

```yaml
environment:
  PGBACKREST_ENABLED: 'false'
  PGBACKREST_RESTORE_ENABLED: 'true'
  PGBACKREST_STANZA: app-prod
  PGBACKREST_S3_ENDPOINT: https://s3.example.internal
  PGBACKREST_S3_BUCKET: postgres-backups
  PGBACKREST_S3_KEY_FILE: /run/secrets/s3-access-key
  PGBACKREST_S3_KEY_SECRET_FILE: /run/secrets/s3-secret-key
```

Restore variables:

| Variable | Description |
| --- | --- |
| `PGBACKREST_RESTORE_ENABLED` | Must be `true` to restore an empty volume. |
| `PGBACKREST_RESTORE_SET` | Optional pgBackRest backup label. Defaults to the latest valid backup. |
| `PGBACKREST_RESTORE_TARGET_TIME` | Optional PostgreSQL recovery timestamp. Restores to that point and promotes the cluster. |

With `PGBACKREST_ENABLED=false`, the restore is run with `--archive-mode=off`, so a recovery test cannot append WAL to the source stanza. To restore and then resume archival on a promoted replacement, set both `PGBACKREST_RESTORE_ENABLED=true` and `PGBACKREST_ENABLED=true`; ensure the former primary is permanently stopped before doing so.

The image can create local PostgreSQL 18 clusters and restore pgBackRest backups. Restoring a cluster made with distro-managed PostgreSQL configuration may require its original `postgresql.conf`, `pg_hba.conf`, or tablespace paths to be made compatible with the container before startup. Test recovery on a representative clean host.

`POSTGRES_USER`, `POSTGRES_PASSWORD`, and `POSTGRES_DB` only initialize an empty database. They do not alter users or passwords in a restored or existing cluster.

## Security And Operations

- Prefer `_FILE` variables or orchestrator secrets over plaintext environment files.
- Keep TLS verification enabled for hosted S3 and configure the endpoint's CA correctly. Disabling verification is only appropriate for a controlled self-signed endpoint on a private network.
- Enable bucket versioning or object locking separately. This image cannot make an object store immutable.
- Use a dedicated bucket prefix/stanza for every independent cluster.
- Test a real restore regularly. A successful backup is not proof of recoverability.
- Use a Docker stop timeout of at least 90 seconds so PostgreSQL can shut down cleanly.

The image is published for `linux/amd64` only.
