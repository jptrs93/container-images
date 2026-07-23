# pg18backrest

`ghcr.io/jptrs93/pg18backrest` is PostgreSQL 18.4 on Debian Bookworm with optional [pgBackRest](https://pgbackrest.org/) backups to one S3-compatible object store. A small Go supervisor is PID 1: it owns PostgreSQL startup, PostgreSQL reconciliation, backup scheduling, and health reporting.

Content tags include the PostgreSQL version, pgBackRest version, and Git release tag, for example `pg18.4_backrest2.58.0-v0.1.2`. PostgreSQL version tags and Git SHA tags are published alongside them.

It preserves the normal `postgres` image initialization interface. When `pgbackrest` is omitted from `config.yaml` or has `enabled: false`, it behaves as PostgreSQL without backup configuration.

## Configure

The image is independent of the runtime orchestrator. Supply these three things using your platform's container, storage, configuration, and secret mechanisms:

- Persistent storage mounted at `/var/lib/postgresql`, not the pre-PostgreSQL-18 `/var/lib/postgresql/data` path. PostgreSQL data is stored at `/var/lib/postgresql/18/docker`; pgBackRest's async spool and health state share the same persistent volume.
- Strict YAML mounted at `POSTGRES_SUPERVISOR_CONFIG`, defaulting to `/etc/postgres-supervisor/config.yaml`. [`config.example.yaml`](config.example.yaml) is the complete schema example.
- Environment variables or mounted secret files selected by the YAML's `env`, `env_value_key`, and `env_file_path_key` fields. Use your platform's secret facility rather than plaintext environment files for production credentials.

[`examples/compose/`](examples/compose/) contains a Docker Compose smoke-test deployment. It is an example only, not a required deployment model.

The S3 bucket must already exist. The stanza separates clusters within the bucket. With `pgbackrest.s3.uri_style: path` and `repo1-path=/`, pgBackRest writes to:

```text
<bucket>/archive/<stanza>/...
<bucket>/backup/<stanza>/...
```

## Supervisor Configuration

The supervisor reads strict YAML from `POSTGRES_SUPERVISOR_CONFIG`, defaulting to `/etc/postgres-supervisor/config.yaml`. Unknown YAML fields are rejected. The file is read at container start; restart the container after changing `initdb`, `settings`, `hba`, or `pgbackrest`.

```yaml
settings:
  max_connections: 100
  shared_buffers: 256MB
hba:
  - host all all 10.0.0.0/8 scram-sha-256
initdb:
  postgres_user:
    env: POSTGRES_USER
  postgres_password:
    env_file_path_key: POSTGRES_PASSWORD_FILE
  postgres_db:
    env: POSTGRES_DB
roles:
  - name:
      value: app_user
      # env: APP_USER
    password:
      env_file_path_key: APP_DATABASE_PASSWORD_FILE
    permissions:
      - database: radkit
        # schema: public # all non-system schemas when omitted
        grants:
          - CREATE
          - USAGE
        table_grants:
          - SELECT
          - INSERT
          - UPDATE
          - DELETE
databases:
  - name: radkit
    owner: app_user
    schemas:
      - staging
    extensions:
      - pgcrypto
```

`settings` is a map of PostgreSQL setting names to scalar YAML values. Strings are safely quoted in the generated `postgresql.conf`; numeric and boolean values are emitted as PostgreSQL scalars. `data_directory`, `hba_file`, `ident_file`, and `config_file` are reserved because the supervisor manages them.

### InitDB And Passwords

`initdb` is optional. When declared, its resolved values are passed to the official entrypoint for an empty `PGDATA`. `postgres_user` defaults to `POSTGRES_USER`, then `postgres`; `postgres_db` defaults to `POSTGRES_DB`, then the resolved `postgres_user`. Those two fields are bootstrap-only and are not changed after initialization.

`initdb.postgres_password` defaults to exactly one of `POSTGRES_PASSWORD` and `POSTGRES_PASSWORD_FILE`. Its password is applied to the resolved bootstrap superuser after every successful PostgreSQL startup, so changing the referenced secret rotates that password on restart. The username is deliberately not renamed or reconciled.

Roles may declare `password` with the same secret-source format. A declared password is applied on every reconciliation; omitting it leaves an existing role password unchanged. Roles, databases, schemas, and extensions remain additive and are never removed automatically. Schema and table grants for configured roles are reconciled to match the YAML configuration.

`pgbackrest` is the source of truth for backup behavior, repository settings, retention, schedules, and the scheduler time zone. See [`config.example.yaml`](config.example.yaml) for a complete example. The section is optional; set `enabled: true` to enable WAL archiving and scheduled backups.

`hba` contains complete `pg_hba.conf` records. The supervisor always prepends `local all all trust` so its local `pgx` reconciliation connection is available inside the container. If `hba` is omitted, the image additionally allows remote `scram-sha-256` password authentication for all users.

PostgreSQL logging is fixed to `stderr` with its logging collector disabled. The supervisor sends PostgreSQL, pgBackRest, and supervisor output to container stdout so `docker logs` and other container log collectors receive one stream.

Each `roles[].name` has exactly one of `value` or `env`. New roles are created with `LOGIN`; declared role passwords are reconciled, but other existing role attributes are not altered. `grants` permits `CREATE` and `USAGE` on a schema. `table_grants` permits `SELECT`, `INSERT`, `UPDATE`, and `DELETE` on existing tables and tables subsequently created by the schema owner. A missing `schema` applies the grant to all current non-system schemas in the specified database.

The reconciler runs with `github.com/jackc/pgx/v5` after PostgreSQL is ready. It creates missing roles, databases, schemas, extensions, and explicit grants, and updates only declared role passwords. It does not delete or alter databases, schemas, roles, extensions, or grants that were removed from YAML. The default local reconciliation user is `initdb.postgres_user`, then `POSTGRES_USER`, then `postgres`; set `POSTGRES_SUPERVISOR_ADMIN_USER` or `POSTGRES_SUPERVISOR_ADMIN_URL` when needed.

## pgBackRest Configuration

When `pgbackrest.enabled` is true, `stanza`, `s3.endpoint`, and `s3.bucket` are required. All other non-secret settings are optional.

| YAML path | Default | Description |
| --- | --- | --- |
| `s3.region` | `us-east-1` | S3 signing region. |
| `s3.uri_style` | `path` | `path` or `host` addressing. |
| `s3.verify_tls` | `true` | Set `false` only for a trusted self-signed endpoint. |
| `retention.full` | `2` | Completed full backup sets retained. pgBackRest needs space for retention plus one additional full backup before expiry. |
| `retention.archive` | `2` | Archive retention count, not a number of days. It retains WAL required by retained backup sets. |
| `process_max` | `4` | Concurrent pgBackRest transfer/compression processes. |
| `archive.push_queue_max` | `1GiB` | Maximum local async WAL queue. Exceeding it drops queued WAL so PostgreSQL stays available, but creates a PITR gap. Monitor it. |
| `archive.timeout_seconds` | `60` | Seconds a backup waits for required WAL to reach S3. |
| `initial_backup` | `true` | Takes a full backup after the stanza's first successful check. |
| `schedules.full` | `0 2 * * 0` | Five-field cron schedule for full backups. |
| `schedules.differential` | `0 2 * * 1-6` | Five-field cron schedule for differential backups. |
| `schedules.check` | `*/5 * * * *` | Five-field cron schedule for `pgbackrest check`. |
| `timezone` | `UTC` | Time zone used for backup schedules. |

The Go scheduler accepts standard five-field cron syntax with ranges, steps, lists, and wildcards. All backup commands run as the `postgres` operating-system user.

### Secret Sources

`initdb.postgres_password`, `roles[].password`, `s3.access_key`, `s3.secret_key`, and optional `repository_cipher_pass` define secret sources rather than secret values. `env_value_key` names an environment variable containing the secret. `env_file_path_key` names an environment variable containing the path to a secret file, suitable for mounted secret projections. Exactly one source must be set for each required secret; empty values and empty files are rejected.

When an entire pgBackRest source declaration is omitted, it defaults to `PGBACKREST_S3_KEY`/`PGBACKREST_S3_KEY_FILE`, `PGBACKREST_S3_KEY_SECRET`/`PGBACKREST_S3_KEY_SECRET_FILE`, or `PGBACKREST_REPO_CIPHER_PASS`/`PGBACKREST_REPO_CIPHER_PASS_FILE`. A partially declared source uses only the listed keys. The example deliberately uses generic `S3_ACCESS_KEY` and `S3_SECRET_KEY` names instead. The supervisor removes configured secret variables from the PostgreSQL child environment, except for the resolved bootstrap values required by the official entrypoint.

## Startup And Health

When `pgbackrest.enabled` is true, PostgreSQL starts immediately. The image then creates and checks the stanza in the background. A temporary S3 outage or invalid remote credentials does not stop PostgreSQL, but the container health check stays unhealthy until the repository check succeeds.

The healthcheck verifies PostgreSQL readiness, successful reconciliation, and the latest pgBackRest maintenance result. It deliberately does not run `pgbackrest check` itself because that command forces a WAL switch. The scheduled check detects repository availability at the configured interval. The supervisor also serves the same result at `GET /healthz` on port `8081`, configurable with `POSTGRES_SUPERVISOR_HEALTH_ADDR`.

Run pgBackRest commands as the `postgres` operating-system user in the running workload:

```bash
pgbackrest --stanza=<stanza> info
```

Force a backup:

```bash
pgbackrest --stanza=<stanza> --type=full backup
pgbackrest --stanza=<stanza> --type=diff backup
```

`pg_isready` only proves that PostgreSQL accepts connections. It does not prove that WAL is reaching the repository. Use `pgbackrest info` and restore tests as operational checks.

## Restore

Restores are explicit. Set `PGBACKREST_RESTORE_ENABLED=true` and use an empty PostgreSQL volume. Restore mode refuses populated `PGDATA` rather than overwriting a cluster.

For an isolated recovery test, set `pgbackrest.enabled: false` in the mounted configuration and enable the explicit restore interlock:

```yaml
environment:
  PGBACKREST_RESTORE_ENABLED: 'true'
```

Restore variables:

| Variable | Description |
| --- | --- |
| `PGBACKREST_RESTORE_ENABLED` | Must be `true` to restore an empty volume. This is intentionally an environment safety interlock. |
| `PGBACKREST_RESTORE_SET` | Optional pgBackRest backup label. Defaults to the latest valid backup. |
| `PGBACKREST_RESTORE_TARGET_TIME` | Optional PostgreSQL recovery timestamp. Restores to that point and promotes the cluster. |

With `pgbackrest.enabled: false`, the restore is run with `--archive-mode=off`, so a recovery test cannot append WAL to the source stanza. To restore and then resume archival on a promoted replacement, set `pgbackrest.enabled: true` as well as `PGBACKREST_RESTORE_ENABLED=true`; ensure the former primary is permanently stopped before doing so.

The image can create local PostgreSQL 18 clusters and restore pgBackRest backups. Restoring a cluster made with distro-managed PostgreSQL configuration may require its original `postgresql.conf`, `pg_hba.conf`, or tablespace paths to be made compatible with the container before startup. Test recovery on a representative clean host.

Without `initdb`, `POSTGRES_USER`, `POSTGRES_PASSWORD`, and `POSTGRES_DB` retain the official image's empty-volume-only behavior. With `initdb`, its declared `postgres_password` is reconciled on an existing or restored cluster; `postgres_user` and `postgres_db` remain bootstrap-only.

## Security And Operations

- Prefer `_FILE` variables or orchestrator secrets over plaintext environment files.
- Keep TLS verification enabled for hosted S3 and configure the endpoint's CA correctly. Disabling verification is only appropriate for a controlled self-signed endpoint on a private network.
- Enable bucket versioning or object locking separately. This image cannot make an object store immutable.
- Use a dedicated bucket prefix/stanza for every independent cluster.
- Test a real restore regularly. A successful backup is not proof of recoverability.
- Configure a container termination grace period of at least 90 seconds so PostgreSQL can shut down cleanly.

The image is published for `linux/amd64` only.
