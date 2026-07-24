#!/usr/bin/env bash
set -Eeuo pipefail

image="${1:-declaritive-postgres:test}"
repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
config_file="$repository_root/declaritive-postgres/tests/config.yaml"
reconciled_config_file="$repository_root/declaritive-postgres/tests/reconciled-config.yaml"
invalid_config_file="$repository_root/declaritive-postgres/tests/invalid-owner-config.yaml"
reconciliation_failure_config_file="$repository_root/declaritive-postgres/tests/reconciliation-failure-config.yaml"
postgres_user_file="$repository_root/declaritive-postgres/tests/postgres-user.txt"
suffix="$(date +%s)"
primary="declaritive-postgres-primary-${suffix}"
invalid="declaritive-postgres-invalid-${suffix}"
reconciliation_failure="declaritive-postgres-reconciliation-failure-${suffix}"
file_user="declaritive-postgres-file-user-${suffix}"
primary_volume="declaritive-postgres-primary-data-${suffix}"

cleanup() {
    docker rm -fv "$file_user" "$reconciliation_failure" "$invalid" "$primary" >/dev/null 2>&1 || true
    docker volume rm "$primary_volume" >/dev/null 2>&1 || true
}

psql_as() {
    local user="$1"
    local password="$2"
    shift 2
    docker exec -e PGPASSWORD="$password" "$primary" psql -h 127.0.0.1 -U "$user" -d app "$@"
}

must_fail() {
    if "$@" >/dev/null 2>&1; then
        printf 'command unexpectedly succeeded: %s\n' "$*" >&2
        return 1
    fi
}

wait_for() {
    local description="$1"
    shift
    local attempt
    for attempt in $(seq 1 90); do
        if "$@" >/dev/null 2>&1; then
            return 0
        fi
        sleep 2
    done
    printf 'timed out waiting for %s\n' "$description" >&2
    "$@" || true
    return 1
}

container_stopped() {
    test "$(docker inspect -f '{{.State.Running}}' "$1")" = false
}

trap cleanup EXIT
docker run --rm --entrypoint sh "$image" -c '! command -v pgbackrest'
docker volume create "$primary_volume" >/dev/null

docker run -d --name "$invalid" --volume "$invalid_config_file":/etc/postgres-supervisor/config.yaml:ro "$image" >/dev/null
wait_for invalid-config-exit container_stopped "$invalid"
invalid_exit_code="$(docker wait "$invalid")"
test "$invalid_exit_code" -ne 0
docker logs "$invalid" | grep -q 'cannot manage grants on schema'

docker run -d --name "$reconciliation_failure" --tmpfs /var/lib/postgresql \
    --volume "$reconciliation_failure_config_file":/etc/postgres-supervisor/config.yaml:ro "$image" >/dev/null
wait_for reconciliation-failure-exit container_stopped "$reconciliation_failure"
reconciliation_failure_exit_code="$(docker wait "$reconciliation_failure")"
test "$reconciliation_failure_exit_code" -ne 0
docker logs "$reconciliation_failure" | grep -q 'PostgreSQL reconciliation failed'

docker run -d --name "$file_user" --tmpfs /var/lib/postgresql \
    -e POSTGRES_USER_FILE=/run/secrets/postgres-user -e POSTGRES_PASSWORD=integration-password \
    --volume "$postgres_user_file":/run/secrets/postgres-user:ro "$image" >/dev/null
wait_for file-user-supervisor-ready docker exec "$file_user" postgres-supervisor healthcheck
docker exec "$file_user" psql -U file_admin -d file_admin -tAc 'select 1' | grep -qx 1
docker stop "$file_user" >/dev/null
test "$(docker inspect -f '{{.State.ExitCode}}' "$file_user")" -eq 0

initial_env=(
    -e INTEGRATION_POSTGRES_PASSWORD=postgres-password
    -e INTEGRATION_OWNER_PASSWORD=owner-password
    -e INTEGRATION_READER_ONE_PASSWORD=reader-one-password
    -e INTEGRATION_READER_TWO_PASSWORD=reader-two-password
    -e INTEGRATION_WRITER_PASSWORD=writer-password
)
docker run -d --name "$primary" --volume "$primary_volume":/var/lib/postgresql \
    --volume "$config_file":/etc/postgres-supervisor/config.yaml:ro \
    "${initial_env[@]}" "$image" -p 5433 >/dev/null

if ! wait_for primary-ready docker exec "$primary" pg_isready -q -U integration_admin -d app; then
    docker logs "$primary"
    exit 1
fi
if ! wait_for supervisor-ready docker exec "$primary" postgres-supervisor healthcheck; then
    docker logs "$primary"
    exit 1
fi
docker exec "$primary" test ! -e /etc/pgbackrest/pgbackrest.conf
docker exec "$primary" psql -U integration_admin -d app -tAc 'show archive_mode' | grep -qx off
docker exec "$primary" psql -U integration_admin -d app -tAc 'show max_connections' | grep -qx 50
docker exec "$primary" psql -U integration_admin -d app -tAc "select has_schema_privilege('reader_one', 'app', 'USAGE')" | grep -qx t
docker exec "$primary" psql -U integration_admin -d app -tAc "select exists(select 1 from pg_namespace where nspname = 'app')" | grep -qx t
docker exec "$primary" psql -U integration_admin -d app -tAc "select exists(select 1 from pg_extension where extname = 'pgcrypto')" | grep -qx t
psql_as app_owner owner-password -c "create table app.records (id integer primary key, value text not null); insert into app.records values (1, 'present');" >/dev/null
psql_as reader_one reader-one-password -tAc 'select value from app.records where id = 1' | grep -qx present
psql_as reader_two reader-two-password -tAc 'select value from app.records where id = 1' | grep -qx present
must_fail psql_as reader_one reader-one-password -c "insert into app.records values (2, 'denied')"
must_fail psql_as reader_one reader-one-password -c "create table app.denied (id integer)"
psql_as app_writer writer-password -c "insert into app.records values (2, 'written'); update app.records set value = 'updated' where id = 2; delete from app.records where id = 2;" >/dev/null

docker stop "$primary" >/dev/null
test "$(docker inspect -f '{{.State.ExitCode}}' "$primary")" -eq 0
docker rm "$primary" >/dev/null
rotated_env=(
    -e INTEGRATION_POSTGRES_PASSWORD=rotated-postgres-password
    -e INTEGRATION_OWNER_PASSWORD=owner-password
    -e INTEGRATION_READER_ONE_PASSWORD=rotated-reader-one-password
    -e INTEGRATION_READER_TWO_PASSWORD=reader-two-password
    -e INTEGRATION_WRITER_PASSWORD=rotated-writer-password
    -e INTEGRATION_AUDIT_READER_PASSWORD=audit-reader-password
)
docker run -d --name "$primary" --volume "$primary_volume":/var/lib/postgresql \
    --volume "$reconciled_config_file":/etc/postgres-supervisor/config.yaml:ro \
    "${rotated_env[@]}" "$image" >/dev/null
if ! wait_for primary-ready-after-rotation docker exec "$primary" pg_isready -q -U integration_admin -d app; then
    docker logs "$primary"
    exit 1
fi
if ! wait_for supervisor-ready-after-rotation docker exec "$primary" postgres-supervisor healthcheck; then
    docker logs "$primary"
    exit 1
fi
if docker exec "$primary" bash -c "PGPASSWORD=postgres-password psql -h 127.0.0.1 -U integration_admin -d app -tAc 'select 1'" >/dev/null 2>&1; then
    printf 'old superuser password still works after rotation\n' >&2
    exit 1
fi
docker exec "$primary" bash -c "PGPASSWORD=rotated-postgres-password psql -h 127.0.0.1 -U integration_admin -d app -tAc 'select 1'" | grep -qx 1
docker exec "$primary" psql -U integration_admin -d app -tAc 'show max_connections' | grep -qx 75
docker exec "$primary" psql -U integration_admin -d app -tAc "select exists(select 1 from pg_namespace where nspname = 'analytics')" | grep -qx t
must_fail psql_as reader_one reader-one-password -tAc 'select 1'
must_fail psql_as app_writer writer-password -tAc 'select 1'
psql_as reader_one rotated-reader-one-password -tAc 'select 1' | grep -qx 1
must_fail psql_as reader_one rotated-reader-one-password -tAc 'select value from app.records where id = 1'
docker exec "$primary" psql -U integration_admin -d app -tAc "select has_schema_privilege('reader_one', 'app', 'USAGE')" | grep -qx f
psql_as reader_two reader-two-password -tAc 'select value from app.records where id = 1' | grep -qx present
psql_as app_writer rotated-writer-password -c "insert into app.records values (2, 'written'); update app.records set value = 'updated' where id = 2;" >/dev/null
must_fail psql_as app_writer rotated-writer-password -c 'delete from app.records where id = 2'
psql_as audit_reader audit-reader-password -tAc 'select value from app.records where id = 2' | grep -qx updated
psql_as app_owner owner-password -c "create table app.reconciled_records (id integer primary key, value text not null); insert into app.reconciled_records values (1, 'reconciled');" >/dev/null
psql_as audit_reader audit-reader-password -tAc 'select value from app.reconciled_records where id = 1' | grep -qx reconciled
must_fail psql_as reader_one rotated-reader-one-password -tAc 'select value from app.reconciled_records where id = 1'

docker stop "$primary" >/dev/null
test "$(docker inspect -f '{{.State.ExitCode}}' "$primary")" -eq 0
