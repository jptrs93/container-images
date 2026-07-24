# declaritive-postgres

`ghcr.io/jptrs93/declaritive-postgres` packages PostgreSQL 18.4 with declarative initialization and reconciliation. Its only image tag matches its Git tag, for example `18.4_v1`.

Mount persistent storage at `/var/lib/postgresql` and YAML at `POSTGRES_SUPERVISOR_CONFIG` (default: `/etc/postgres-supervisor/config.yaml`). The image is `linux/amd64` only. [`examples/compose/`](examples/compose/) is a local smoke-test deployment.

## Configuration

YAML is strict: unknown fields fail startup. A string exactly equal to `${ENV_NAME}` is replaced once with the non-empty environment value. Use your orchestrator's secret support for those variables.

If no configuration file is mounted, the normal official `postgres` initialization variables apply. Declaring `initdb` requires `postgres_password`; its password is reconciled on every startup. `postgres_user` and `postgres_db` only affect an empty volume.

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
