# declarative-postgres-backrest

`ghcr.io/jptrs93/declarative-postgres-backrest` packages PostgreSQL and [pgBackRest](https://pgbackrest.org/) for S3 backups into a self contained image that is configured via a single declaritive yaml file. The configuration file is reconciled against the database on each start up. In addition to standard static postgres configuration, the yaml file lets you declaritively define databases, roles/ users, credentials, permissions, extensions etc..  At any time you can change the config file and the database state will be reconciled to match on next restart. This approach aligns more cleanly with modern deployment orchestration platforms. For example, you can rotate a password or audit what users and credentials exist without executing SQL commands within the instance itself. 

Release Git tags use `declarative-postgres-backrest/<postgres version>_<backrest version>_<this version>`, for example `declarative-postgres-backrest/18.4_2.58.0_v1`. The directory prefix is removed from the published container tag, producing `18.4_2.58.0_v1`.

## Usage

To run the image you need to:

* Mount persistent storage at the container path `/var/lib/postgresql` (standard for postgres > 18).
* Mount the declarative YAML configuration file at `/etc/postgres-supervisor/config.yaml`. If you want to change were the config file is loaded from then you can set `POSTGRES_SUPERVISOR_CONFIG`.

YAML is strict: unknown fields fail startup. A string exactly equal to `${ENV_NAME}` is replaced once with the non-empty environment value. `${ENV_NAME:-default}` uses `default` when the variable is unset or empty. Use your orchestrator's secret support for those variables.

`settings.listen_addresses`, `settings.port`, and `settings.unix_socket_directories` are optional and default to `*`, `5432`, and `/var/run/postgresql`. These YAML values override conflicting PostgreSQL command-line options. The supervisor and pgBackRest use the configured port and first socket directory. Custom socket directories must already exist and be writable by the `postgres` operating-system user; publish the configured port separately through your container runtime when remote access is required.

The supervisor connects as `initdb.postgres_user`, falling back to `POSTGRES_USER` and then `postgres`. Set `POSTGRES_SUPERVISOR_ADMIN_USER` when an existing volume requires a different PostgreSQL superuser.

If no configuration file is mounted, the normal official `postgres` initialization variables apply. Declaring `initdb` requires `postgres_password`; its password is reconciled on every startup. `postgres_user` and `postgres_db` only affect an empty volume.

## Examples

### Minimal postgres only

If you want the minimal possible setup with a single super user and database:

```yaml
settings:
  max_connections: 100
  shared_buffers: 256MB
hba:
  - host all all 10.0.0.0/8 scram-sha-256
initdb:
  postgres_user: app_admin
  postgres_password: ${POSTGRES_PASSWORD}
  postgres_db: app_db
```

### PostgreSQL Only

This complete configuration creates an application owner, a read-only role, the `app_db` database, and a `staging` schema. Omitting `pgbackrest` disables backup management.

```yaml
settings:
  max_connections: 100
  shared_buffers: 256MB
  shared_preload_libraries: pg_stat_statements
hba:
  - host all app_user all scram-sha-256
  - host all reader all scram-sha-256
initdb:
  postgres_user: postgres
  postgres_password: ${POSTGRES_PASSWORD}
  postgres_db: postgres
roles:
  - name: app_user
    password: ${APP_PASSWORD}
  - name: reader
    password: ${READER_PASSWORD}
    permissions:
      - database: app_db
        grants: [USAGE]
        table_grants: [SELECT]
databases:
  - name: app_db
    owner: app_user
    schemas: [staging]
    extensions: [pgcrypto, pg_stat_statements]
```

### PostgreSQL With pgBackRest

This complete configuration enables WAL archiving, scheduled backups, and S3 repository checks. The bucket must already exist. With `uri_style: path`, backups are stored under `<bucket>/archive/<stanza>/` and `<bucket>/backup/<stanza>/`.

```yaml
settings:
  max_connections: 100
  shared_buffers: 256MB
  shared_preload_libraries: pg_stat_statements
hba:
  - host all app_user all scram-sha-256
  - host all reader all scram-sha-256
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
    ca_file: /etc/ssl/certs/s3-ca.crt
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
  - name: app_user
    password: ${APP_PASSWORD}
  - name: reader
    password: ${READER_PASSWORD}
    permissions:
      - database: app_db
        schema: public
        grants: [USAGE]
        table_grants: [SELECT]
databases:
  - name: app_db
    owner: app_user
    schemas: [staging]
    extensions: [pgcrypto, pg_stat_statements]
```

### Group roles for read and write access

The pattern below separates *what access exists* from *who has it*. `app_read` and `app_write` hold the grants but cannot log in, so they are pure bundles of privilege. `app_user` and `debug_user` log in and are granted whichever bundles they need, which makes an application's access a one-line statement rather than a list of privileges repeated per user.

```yaml
settings:
  max_connections: 100
  shared_buffers: 256MB
hba:
  - host all app_user all scram-sha-256
  - host all debug_user all scram-sha-256
initdb:
  postgres_user: postgres
  postgres_password: ${POSTGRES_PASSWORD}
  postgres_db: postgres
roles:
  # Group roles: no password, no login, they exist only to be granted onward.
  - name: app_read
    nologin: true
    permissions:
      - database: app_db
        schema: public
        grants: [USAGE]
        table_grants: [SELECT]
        sequence_grants: [SELECT]
        routine_grants: [EXECUTE]

  - name: app_write
    nologin: true
    permissions:
      - database: app_db
        schema: public
        table_grants: [INSERT, UPDATE, DELETE]
        sequence_grants: [USAGE]

  # Login roles: they hold no permissions of their own, only memberships.
  - name: app_user
    password: ${APP_PASSWORD}
    roles: [app_read, app_write]

  - name: debug_user
    password: ${DEBUG_PASSWORD}
    roles: [app_read]

databases:
  - name: app_db
    extensions: [pgcrypto]
```

`app_db` has no `owner`, so it stays with the bootstrap superuser, which then creates the tables. That is fine here: because grants are registered against every role that can create objects, `app_read` and `app_write` cover the superuser's tables without anything further being declared.

`app_write` deliberately omits `grants: [USAGE]` on the schema. `app_user` gets schema access through `app_read`, and revoking `app_read` from a role that still holds `app_write` should leave it unable to reach the schema at all. Give `app_write` its own `USAGE` if you intend it to stand alone.

To take away a user's write access, delete `app_write` from its `roles` list and restart. To retire a user, remove the role from the YAML; note that the role itself is not dropped, but the memberships and grants this configuration gave it are revoked.

### Every grant in one place

A permission names a database, optionally a schema, and then any combination of the four object classes. This role receives everything the vocabulary can express:

```yaml
roles:
  - name: full_access
    password: ${FULL_ACCESS_PASSWORD}
    permissions:
      - database: app_db
        schema: public
        grants: [CREATE, USAGE]
        table_grants: [SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER, MAINTAIN]
        sequence_grants: [USAGE, SELECT, UPDATE]
        routine_grants: [EXECUTE]
```

Each list also accepts the single value `ALL`, which is shorter and stays correct if PostgreSQL adds a privilege later:

```yaml
roles:
  - name: full_access
    password: ${FULL_ACCESS_PASSWORD}
    permissions:
      - database: app_db
        schema: public
        grants: [ALL]
        table_grants: [ALL]
        sequence_grants: [ALL]
        routine_grants: [ALL]
```

Omitting `schema` applies the permission to every non-system schema in the database, and a role may hold several permissions covering different databases or schemas:

```yaml
roles:
  - name: analyst
    password: ${ANALYST_PASSWORD}
    permissions:
      # Every schema in app_db, current and future.
      - database: app_db
        grants: [USAGE]
        table_grants: [SELECT]
      # Write access confined to one schema of another database.
      - database: warehouse
        schema: staging
        grants: [USAGE, CREATE]
        table_grants: [ALL]
        sequence_grants: [ALL]
```

### Memberships and predefined roles

`roles` grants a role membership in other roles. The members are cluster-wide, so unlike `permissions` a membership is not scoped to one database:

```yaml
roles:
  - name: metrics
    password: ${METRICS_PASSWORD}
    default_roles: [pg_monitor]   # read every statistics view, including query text
  - name: backup_operator
    password: ${BACKUP_PASSWORD}
    default_roles: [pg_read_all_data]   # SELECT on every table in every database
```

Useful PostgreSQL predefined roles include `pg_read_all_data`, `pg_write_all_data`, `pg_read_all_stats`, `pg_monitor`, `pg_signal_backend`, and `pg_maintain`. Reading query text from `pg_stat_statements` needs `pg_read_all_stats` or `pg_monitor`; without it the `query` column reads `<insufficient privilege>` for statements belonging to other roles.

Avoid `pg_read_server_files`, `pg_write_server_files`, and `pg_execute_server_program` unless you mean to grant superuser: each is a path to running code as the `postgres` operating-system user.


## Lifecycle

- The container entrypoint is a supervisor process.
- The supervisor first parses and validates the configuration, generates the PostgreSQL configuration files, and then starts the main PostgreSQL process. If the configuration is invalid, the container crashes.
- If the main PostgreSQL process dies at any point, the supervisor propagates its failure and the container crashes.
- Once the PostgreSQL instance is running, the supervisor runs the reconciler to reconcile users and roles, permissions, and other declared database state. If reconciliation fails, the container crashes.
- After successful reconciliation, the readiness signal is given.
- When pgBackRest is enabled, PostgreSQL invokes it to archive live WAL segments, and the supervisor invokes it periodically for full and differential backups.
- pgBackRest failures and errors do not crash the container, but the container is marked unhealthy and the errors appear in its logs.
- Everything the container writes to stdout — supervisor, PostgreSQL, and pgBackRest output alike — is one JSON object per line; see [Logging](#logging).

`pgbackrest.stanza`, `pgbackrest.s3.host`, and `pgbackrest.s3.bucket` are required when enabled. `s3.port` defaults to `443`; use `9000` and `verify_tls: false` only for a trusted HTTP MinIO endpoint. `s3.ca_file` is an optional absolute path to a PEM CA file used to verify the S3 endpoint certificate; mount the file into the container and keep `verify_tls: true`. Other omitted pgBackRest values use the defaults in [`config.example.yaml`](config.example.yaml).

## Reconciliation

The supervisor creates missing roles, databases, schemas, and extensions. Declared role passwords are updated; omitted passwords are left unchanged. Roles, databases, schemas, and extensions removed from YAML are not deleted.

`nologin` defaults to `false`, so a role with a name and a password behaves as it always has. Setting `nologin: true` produces a `NOLOGIN` role, which cannot connect and exists to be granted onward as a bundle of privileges. It is reconciled like everything else: flipping the value on an existing role takes its ability to connect away, or gives it back. Declaring a `password` on a role that cannot log in is rejected rather than silently ignored.

`replication: true` gives a role the `REPLICATION` attribute, which the walsender connection behind streaming or logical replication requires. Like `nologin` it is reconciled in both directions: removing it strips the attribute, including one granted by hand, so the file stays the whole truth about who can replicate. PostgreSQL does not pass the attribute through role membership, so it belongs on the role that actually connects; declaring it on a `nologin` role is rejected.

`roles` and `default_roles` both grant a role membership in other roles. They are one mechanism split into two lists purely so a configuration states where a privilege comes from: `roles` names roles this configuration defines, and `default_roles` names the roles PostgreSQL ships, such as `pg_read_all_data`, `pg_write_all_data` and `pg_monitor`.

The split is checked rather than advisory. PostgreSQL reserves the `pg_` prefix and refuses to create any role that uses it, so the prefix separates the two lists exactly: a `pg_` name in `roles`, or a non-`pg_` name in `default_roles`, is rejected at startup with a message naming the list it belongs in.

A member inherits the privileges of every role it holds, including those granted after the fact, so membership is the natural way to bundle access. Roles are cluster-wide, so unlike `permissions` a membership is not confined to one database. Memberships not listed in either field are revoked, so removing an entry converges. `pg_database_owner` is rejected because PostgreSQL does not allow explicit members; set it as a database `owner` instead. Granting `pg_read_server_files`, `pg_write_server_files` or `pg_execute_server_program` is effectively equivalent to granting superuser, since each provides a path to running code as the `postgres` operating-system user.

`owner` is optional and defaults to the bootstrap superuser, which is `initdb.postgres_user` when declared and otherwise `POSTGRES_USER` or `postgres`. It is reconciled on every startup, not only at creation: a database or declared schema whose owner has drifted from the YAML is handed back to the declared role. Changing `owner` does not transfer ownership of tables, sequences, or other objects already inside the database, which stay with their original owner. Use `REASSIGN OWNED BY <old> TO <new>` to move those.

Grants mirror the PostgreSQL object classes one for one, so the YAML says exactly what the database is told:

| Field | Object class | Accepted privileges |
| --- | --- | --- |
| `grants` | schemas | `CREATE`, `USAGE` |
| `table_grants` | tables, views, materialized views, partitioned tables, foreign tables | `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `REFERENCES`, `TRIGGER`, `MAINTAIN` |
| `sequence_grants` | sequences | `USAGE`, `SELECT`, `UPDATE` |
| `routine_grants` | functions, procedures, aggregates | `EXECUTE` |

Every list also accepts the single value `ALL`, which cannot be combined with named privileges. An omitted `schema` applies to every non-system schema in the database. A configured role cannot manage grants on a schema it owns.

Each class is declared outright and none is inferred from another. In particular a role that inserts into a table with a `serial` column needs `sequence_grants: [USAGE]` as well as `table_grants: [INSERT]`, because PostgreSQL checks the sequence separately. Columns declared `generated always as identity` need no sequence privilege.

### Grants cover existing and future objects alike

A grant means the same thing whenever it is applied relative to the objects it covers. There is no separate notion of a default privilege in this configuration: `table_grants: [SELECT]` covers the tables that exist now and the tables created tomorrow, and removing it revokes both.

PostgreSQL itself does not work that way. `ALTER DEFAULT PRIVILEGES` is scoped to the role that creates an object, so a default privilege registered for one role does nothing about objects another role creates. The reconciler closes that gap by treating the YAML as the whole truth: on every startup it asks the catalog which roles can create objects in each schema, using `has_schema_privilege`, which accounts for the schema owner, superusers, roles granted `CREATE` outright, and roles that inherit `CREATE` through `roles`. Each grant is then registered against every one of those roles.

Two consequences follow. Grants apply regardless of who creates an object, including the bootstrap superuser, so the common pattern of letting the superuser own every table while an application role holds `ALL` works without further configuration. And the creator set is re-read before anything is revoked, so when a role loses `CREATE`, or leaves the configuration entirely, the default privileges registered for it are cleaned up rather than left behind.

Reconciliation reads before it writes. Every startup first compares what the cluster holds against what the file asks for, and when the two already agree the phase is read-only: no grant or revoke is issued at all, which is the normal case for a restart with an unchanged file. When something has drifted the privileges are revoked and re-granted outright rather than patched, so the result converges from whatever state the database was in. Each role's rewrite within one database runs as a single transaction, so a client connecting while reconciliation is running sees either the old privileges or the new ones, never the moment in between. A successful run logs one line saying which of the two happened, so `docker logs` distinguishes a no-op restart from a rewrite:

```
{"time":"2026-08-23T04:05:06.789Z","level":"INFO","msg":"PostgreSQL reconciliation complete: 4 roles, 2 databases, privileges unchanged"}
```

Roles that can create objects outside the configuration are the one thing this cannot account for. A role granted `CREATE` by hand becomes a creator that the next reconcile will discover and include, but objects it creates before then are covered only from that reconcile onward.

The supervisor always adds `local all all trust` for its local reconciliation connection. If `hba` is omitted, it also permits remote `scram-sha-256` access.

## Logging

Container stdout carries one JSON object per line, with supervisor, PostgreSQL, and pgBackRest records intermixed and nothing written to disk. Every record is flat — no envelope or nesting — and uses the Go standard keys: `time` (RFC3339, UTC), `level`, and `msg`.

PostgreSQL runs with the logging collector emitting `jsonlog` into named pipes that the supervisor relays, so a multi-line statement or stack trace stays inside one record instead of fragmenting across lines. Each record's `timestamp`, `error_severity`, and `message` keys are renamed to the standard three, and every other jsonlog field (`pid`, `user`, `dbname`, `application_name`, `state_code`, `backend_type`, `statement`, `detail`, `hint`, ...) passes through unchanged:

```
{"time":"2026-08-23T04:05:06.789Z","level":"ERROR","msg":"relation \"missing\" does not exist","backend_type":"client backend","dbname":"app","pid":123,"state_code":"42P01","statement":"select * from missing","user":"app_user"}
```

Only the keys are renamed, never the values: `level` on a PostgreSQL record carries the PostgreSQL severity verbatim (`DEBUG1` .. `DEBUG5`, `LOG`, `INFO`, `NOTICE`, `WARNING`, `ERROR`, `FATAL`, `PANIC`), while supervisor records use the Go slog values (`DEBUG`, `INFO`, `WARN`, `ERROR`).

Plain-text output — initdb, pgBackRest, anything a process writes straight to stderr — is wrapped line by line as `msg`, with the level read from severity markers such as `FATAL:` or `P00   WARN:` when present:

```
{"time":"2026-08-23T02:00:00.123456789Z","level":"INFO","msg":"P00   INFO: archive-push command begin"}
```

pgBackRest reaches stdout by two routes. Commands the supervisor runs itself — stanza-create, check, backup, restore — have their console output captured directly. The archive commands cannot be captured that way: archive-push and archive-get run under PostgreSQL, and each spawns an async worker that detaches from everything. Those commands instead log at `detail` level through `/var/log/pgbackrest`, where the supervisor has replaced their log files with named pipes it relays — so async archive errors that pgBackRest would otherwise bury in a file (`unable to push WAL file ... check '...-archive-push-async.log' for errors`) appear on stdout like everything else, and still nothing is written to disk. `detail`-marked lines relay at level `DEBUG`.

Because every line is JSON, `docker logs` output can be filtered directly, for example `docker logs app 2>&1 | jq 'select(.level == "ERROR")'`, and shippers such as Loki, Vector, or Fluent Bit ingest it with a plain JSON parser and no multiline configuration.

The transport settings (`logging_collector`, `log_destination`, `log_directory`, `log_filename`, `log_rotation_age`, `log_rotation_size`, `log_file_mode`, `log_timezone`) are managed by the supervisor and rejected in `settings`, so the format cannot be broken by configuration. Content settings such as `log_min_messages`, `log_statement`, and `log_min_duration_statement` remain yours to set.

## Backup And Restore

With pgBackRest enabled, PostgreSQL starts even if S3 is unavailable, but health remains unhealthy until the repository check succeeds. Health covers PostgreSQL readiness, reconciliation, and the latest backup maintenance result. It is available through the Docker health check and `GET /healthz` on port `8081`.

`s3.host` accepts a hostname, an IPv4 address, or an IPv6 address. An IPv6 address cannot be handed to pgBackRest directly — it places the endpoint verbatim in the HTTP `Host` header, where an unbracketed IPv6 literal is illegal and S3 servers reject it (`InvalidRequest ... too many colons in address`) — so the supervisor maps the address to the hosts-file alias `pgbackrest-s3-endpoint` and configures that name instead. With `verify_tls: true` the server certificate is then checked against the alias, so a certificate for an IPv6 endpoint must include `pgbackrest-s3-endpoint` as a subject alternative name.

Run backup commands as the `postgres` operating-system user:

```bash
pgbackrest --stanza=app-production info
pgbackrest --stanza=app-production --type=full backup
pgbackrest --stanza=app-production --type=diff backup
```

Restore requires an empty PostgreSQL volume and `PGBACKREST_RESTORE_ENABLED=true`. `PGBACKREST_RESTORE_SET` selects a backup label; `PGBACKREST_RESTORE_TARGET_TIME` selects a recovery time. Set `pgbackrest.enabled: false` for an isolated restore test so it cannot archive to the source stanza. Set it to `true` only when the former primary is permanently stopped and the restored instance will become the replacement primary.

Keep S3 TLS verification enabled where possible, use a separate stanza per cluster, enable bucket versioning or object locking independently, and test restores regularly.
