# declaritive-postgres

`ghcr.io/jptrs93/declaritive-postgres` packages PostgreSQL into a self-contained image that is configured via a single declarative YAML file. The configuration file is reconciled against the database on each startup. In addition to standard static PostgreSQL configuration, the YAML file lets you declaratively define databases, roles/users, credentials, permissions, extensions, and more. At any time, you can change the configuration file and the database state will be reconciled to match on the next restart. This approach aligns cleanly with modern deployment orchestration platforms. For example, you can rotate a password or audit which users and credentials exist without executing SQL commands within the instance itself.

Release Git tags use `declaritive-postgres/<postgres version>_<this version>`, for example `declaritive-postgres/18.4_v1`. The directory prefix is removed from the published container tag, producing `18.4_v1`.

## Usage

To run the image you need to:

* Mount persistent storage at the container path `/var/lib/postgresql` (standard for PostgreSQL >= 18).
* Mount the declarative YAML configuration file at `/etc/postgres-supervisor/config.yaml`. To load the configuration from another path, set `POSTGRES_SUPERVISOR_CONFIG`.

The image is `linux/amd64` only. [`examples/compose/`](examples/compose/) provides a local smoke-test deployment.

YAML is strict: unknown fields fail startup. A string exactly equal to `${ENV_NAME}` is replaced once with the non-empty environment value. `${ENV_NAME:-default}` uses `default` when the variable is unset or empty. Use your orchestrator's secret support for those variables.

`settings.listen_addresses`, `settings.port`, and `settings.unix_socket_directories` are optional and default to `*`, `5432`, and `/var/run/postgresql`. These YAML values override conflicting PostgreSQL command-line options. The supervisor uses the configured port and first socket directory for readiness and reconciliation. Custom socket directories must already exist and be writable by the `postgres` operating-system user; publish the configured port separately through your container runtime when remote access is required.

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
  postgres_db: app
```

### Declarative application database

Bootstrap the administrative `postgres` database, then let reconciliation create `app` with the non-superuser `app` as its owner:

```yaml
settings:
  max_connections: 100
  shared_buffers: 256MB
  shared_preload_libraries: pg_stat_statements
hba:
  - host all app all scram-sha-256
  - host all reader all scram-sha-256
initdb:
  postgres_user: postgres
  postgres_password: ${POSTGRES_PASSWORD}
  postgres_db: postgres
roles:
  - name: app
    password: ${APP_PASSWORD}
  - name: reader
    password: ${READER_PASSWORD}
    permissions:
      - database: app
        grants:
          - USAGE
        table_grants:
          - SELECT
databases:
  - name: app
    owner: app
    schemas:
      - staging
    extensions:
      - pgcrypto
      - pg_stat_statements
```

## Lifecycle

- The container entrypoint is a supervisor process.
- The supervisor first parses and validates the configuration, generates the PostgreSQL configuration files, and then starts the main PostgreSQL process. If the configuration is invalid, the container crashes.
- If the main PostgreSQL process dies at any point, the supervisor propagates its failure and the container crashes.
- Once the PostgreSQL instance is running, the supervisor runs the reconciler to reconcile users and roles, permissions, and other declared database state. If reconciliation fails, the container crashes.
- After successful reconciliation, the readiness signal is given.

## Reconciliation

`member_of` grants a role membership in other roles, including the PostgreSQL predefined roles such as `pg_read_all_data`, `pg_write_all_data` and `pg_monitor`. Membership covers objects created later without a reconcile, so it is the simplest way to give a role blanket read or write access, but roles are cluster-wide and a membership therefore applies to every database. Memberships not listed are revoked, so removing an entry converges. `pg_database_owner` is rejected because PostgreSQL does not allow explicit members; set it as a database `owner` instead. Granting `pg_read_server_files`, `pg_write_server_files` or `pg_execute_server_program` is effectively equivalent to granting superuser, since each provides a path to running code as the `postgres` operating-system user.

The supervisor creates missing roles, databases, schemas, and extensions. Declared role passwords are updated; omitted passwords are left unchanged. Roles, databases, schemas, and extensions removed from YAML are not deleted.

`owner` is optional and defaults to the bootstrap superuser, which is `initdb.postgres_user` when declared and otherwise `POSTGRES_USER` or `postgres`. It is reconciled on every startup, not only at creation: a database or declared schema whose owner has drifted from the YAML is handed back to the declared role. Changing `owner` does not transfer ownership of tables, sequences, or other objects already inside the database, which stay with their original owner. Use `REASSIGN OWNED BY <old> TO <new>` to move those.

`grants` accepts `CREATE` and `USAGE`, the complete set of PostgreSQL schema privileges. `table_grants` accepts `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `REFERENCES`, `TRIGGER`, and `MAINTAIN`. Either list also accepts the single value `ALL`, which cannot be combined with named privileges. They are reconciled for the configured role. An omitted `schema` applies to every non-system schema in the database. A configured role cannot manage grants on a schema it owns.

Sequence privileges are not declared directly, they follow from `table_grants`. `INSERT` also grants `USAGE` on the schema's sequences, and `ALL` grants `ALL`, because PostgreSQL treats sequences as a separate object class: without this a role holding `INSERT` still cannot insert into a table with a `serial` column. Columns declared `generated always as identity` need no sequence privilege.

Grants are applied to the objects that exist at reconcile time and registered as default privileges for the schema owner, so objects that owner creates later are covered without a restart. Objects created by any other role are covered at the next reconcile.

The supervisor always adds `local all all trust` for its local reconciliation connection. If `hba` is omitted, it also permits remote `scram-sha-256` access. PostgreSQL and supervisor logs go to container stdout.

## Health And Operations

Health covers PostgreSQL readiness and successful reconciliation. It is available through the Docker health check and `GET /healthz` on port `8081`.

Treat write access to the YAML as database-administrator access. It can create roles and databases, grant configured privileges, rotate the bootstrap-superuser password, and set PostgreSQL configuration. Restrict write access to the mounted configuration and its referenced secrets.
