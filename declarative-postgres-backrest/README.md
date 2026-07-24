# declarative-postgres-backrest

`ghcr.io/jptrs93/declarative-postgres-backrest` packages PostgreSQL and [pgBackRest](https://pgbackrest.org/) for S3 backups into a self contained image that is configured via a single declaritive yaml file. The configuration file is reconciled against the database on each start up. In addition to standard static postgres configuration, the yaml file lets you declaritively define databases, roles/ users, credentials, permissions, extensions etc..  At any time you can change the config file and the database state will be reconciled to match on next restart. This approach aligns more cleanly with modern deployment orchestration platforms. For example, you can rotate a password or audit what users and credentials exist without executing SQL commands within the instance itself. 

Images are tagged as `<postgres version>_<backrest version>_<this version>`, for example `18.4_2.58.0_v1`.

## Usage

To run the image you need to:

* Mount persistent storage at the container path `/var/lib/postgresql` (standard for postgres > 18).
* Mount the declarative YAML configuration file at `/etc/postgres-supervisor/config.yaml`. If you want to change were the config file is loaded from then you can set `POSTGRES_SUPERVISOR_CONFIG`.

YAML is strict: unknown fields fail startup. A string exactly equal to `${ENV_NAME}` is replaced once with the non-empty environment value. Use your orchestrator's secret support for those variables.

If no configuration file is mounted, the normal official `postgres` initialization variables apply. Declaring `initdb` requires `postgres_password`; its password is reconciled on every startup. `postgres_user` and `postgres_db` only affect an empty volume.

## Examples

### Minimal postgres only

If you want the minimal possible setup with a single super user and database:

```yaml
initdb:
  postgres_user: app_admin
  postgres_password: ${POSTGRES_PASSWORD}
  postgres_db: app
```

### PostgreSQL Only

This complete configuration creates an application owner, a read-only role, the `app` database, and a `staging` schema. Omitting `pgbackrest` disables backup management.

```yaml
settings:
  max_connections: 100
  shared_buffers: 256MB
hba:
  - host all all 10.0.0.0/8 scram-sha-256
initdb:
  postgres_user: postgres
  postgres_password: ${POSTGRES_PASSWORD}
  postgres_db: postgres
roles:
  - name: app_owner
    password: ${APP_OWNER_PASSWORD}
  - name: app_reader
    password: ${APP_READER_PASSWORD}
    permissions:
      - database: app
        grants:
          - USAGE
        table_grants:
          - SELECT
databases:
  - name: app
    owner: app_owner
    schemas:
      - staging
    extensions:
      - pgcrypto
```

### PostgreSQL With pgBackRest

This complete configuration enables WAL archiving, scheduled backups, and S3 repository checks. The bucket must already exist. With `uri_style: path`, backups are stored under `<bucket>/archive/<stanza>/` and `<bucket>/backup/<stanza>/`.

```yaml
settings:
  max_connections: 100
  shared_buffers: 256MB
hba:
  - host all all 10.0.0.0/8 scram-sha-256
initdb:
  postgres_user: postgres
  postgres_password: ${POSTGRES_PASSWORD}
  postgres_db: postgres
pgbackrest:
  enabled: true
  stanza: app-production
  repository_cipher_pass: ${PGBACKREST_REPOSITORY_CIPHER_PASS}
  s3:
    host: s3.example.com
    port: '443'
    bucket: postgres-backups
    region: us-east-1
    uri_style: path
    verify_tls: true
    access_key: ${S3_ACCESS_KEY}
    secret_key: ${S3_SECRET_KEY}
  retention:
    full: 2
    archive: 2
  schedules:
    full: '0 2 * * 0'
    differential: '0 2 * * 1-6'
    check: '*/5 * * * *'
  archive:
    push_queue_max: 1GiB
    timeout_seconds: 60
  process_max: 4
  initial_backup: true
  timezone: UTC
roles:
  - name: app_owner
    password: ${APP_OWNER_PASSWORD}
  - name: app_reader
    password: ${APP_READER_PASSWORD}
    permissions:
      - database: app
        schema: public
        grants:
          - USAGE
        table_grants:
          - SELECT
databases:
  - name: app
    owner: app_owner
    schemas:
      - staging
    extensions:
      - pgcrypto
```

`pgbackrest.stanza`, `pgbackrest.s3.host`, and `pgbackrest.s3.bucket` are required when enabled. `s3.port` defaults to `443`; use `9000` and `verify_tls: false` only for a trusted HTTP MinIO endpoint. Other omitted pgBackRest values use the defaults in [`config.example.yaml`](config.example.yaml).

## Reconciliation

The supervisor creates missing roles, databases, schemas, and extensions. Declared role passwords are updated; omitted passwords are left unchanged. Roles, databases, schemas, and extensions removed from YAML are not deleted.

`grants` accepts `CREATE` and `USAGE`; `table_grants` accepts `SELECT`, `INSERT`, `UPDATE`, and `DELETE`. They are reconciled for the configured role. An omitted `schema` applies to every non-system schema in the database. A configured role cannot manage grants on a schema it owns.

The supervisor always adds `local all all trust` for its local reconciliation connection. If `hba` is omitted, it also permits remote `scram-sha-256` access. PostgreSQL, pgBackRest, and supervisor logs go to container stdout.

## Backup And Restore

With pgBackRest enabled, PostgreSQL starts even if S3 is unavailable, but health remains unhealthy until the repository check succeeds. Health covers PostgreSQL readiness, reconciliation, and the latest backup maintenance result. It is available through the Docker health check and `GET /healthz` on port `8081`.

Run backup commands as the `postgres` operating-system user:

```bash
pgbackrest --stanza=app-production info
pgbackrest --stanza=app-production --type=full backup
pgbackrest --stanza=app-production --type=diff backup
```

Restore requires an empty PostgreSQL volume and `PGBACKREST_RESTORE_ENABLED=true`. `PGBACKREST_RESTORE_SET` selects a backup label; `PGBACKREST_RESTORE_TARGET_TIME` selects a recovery time. Set `pgbackrest.enabled: false` for an isolated restore test so it cannot archive to the source stanza. Set it to `true` only when the former primary is permanently stopped and the restored instance will become the replacement primary.

Keep S3 TLS verification enabled where possible, use a separate stanza per cluster, enable bucket versioning or object locking independently, and test restores regularly.
