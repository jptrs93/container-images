# declaritive-postgres

`ghcr.io/jptrs93/declaritive-postgres` packages PostgreSQL into a self-contained image that is configured via a single declarative YAML file. The configuration file is reconciled against the database on each startup. In addition to standard static PostgreSQL configuration, the YAML file lets you declaratively define databases, roles/users, credentials, permissions, extensions, and more. At any time, you can change the configuration file and the database state will be reconciled to match on the next restart. This approach aligns cleanly with modern deployment orchestration platforms. For example, you can rotate a password or audit which users and credentials exist without executing SQL commands within the instance itself.

Release Git tags use `declaritive-postgres/<postgres version>_<this version>`, for example `declaritive-postgres/18.4_v1`. The directory prefix is removed from the published container tag, producing `18.4_v1`.

## Usage

To run the image you need to:

* Mount persistent storage at the container path `/var/lib/postgresql` (standard for PostgreSQL >= 18).
* Mount the declarative YAML configuration file at `/etc/postgres-supervisor/config.yaml`. To load the configuration from another path, set `POSTGRES_SUPERVISOR_CONFIG`.

The image is `linux/amd64` only. [`examples/compose/`](examples/compose/) provides a local smoke-test deployment.

YAML is strict: unknown fields fail startup. A string exactly equal to `${ENV_NAME}` is replaced once with the non-empty environment value. Use your orchestrator's secret support for those variables.

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

Bootstrap the administrative `postgres` database, then let reconciliation create `app` with the non-superuser `app_owner` as its owner:

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

## Lifecycle

- The container entrypoint is a supervisor process.
- The supervisor first parses and validates the configuration, generates the PostgreSQL configuration files, and then starts the main PostgreSQL process. If the configuration is invalid, the container crashes.
- If the main PostgreSQL process dies at any point, the supervisor propagates its failure and the container crashes.
- Once the PostgreSQL instance is running, the supervisor runs the reconciler to reconcile users and roles, permissions, and other declared database state. If reconciliation fails, the container crashes.
- After successful reconciliation, the readiness signal is given.

## Reconciliation

The supervisor creates missing roles, databases, schemas, and extensions. Declared role passwords are updated; omitted passwords are left unchanged. Roles, databases, schemas, and extensions removed from YAML are not deleted.

`grants` accepts `CREATE` and `USAGE`; `table_grants` accepts `SELECT`, `INSERT`, `UPDATE`, and `DELETE`. They are reconciled for the configured role. An omitted `schema` applies to every non-system schema in the database. A configured role cannot manage grants on a schema it owns.

The supervisor always adds `local all all trust` for its local reconciliation connection. If `hba` is omitted, it also permits remote `scram-sha-256` access. PostgreSQL and supervisor logs go to container stdout.

## Health And Operations

Health covers PostgreSQL readiness and successful reconciliation. It is available through the Docker health check and `GET /healthz` on port `8081`.

Treat write access to the YAML as database-administrator access. It can create roles and databases, grant configured privileges, rotate the bootstrap-superuser password, and set PostgreSQL configuration. Restrict write access to the mounted configuration and its referenced secrets.
