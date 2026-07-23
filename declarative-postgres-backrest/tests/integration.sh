#!/usr/bin/env bash
set -Eeuo pipefail

image="${1:-declarative-postgres-backrest:test}"
repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
config_file="$repository_root/declarative-postgres-backrest/tests/config.yaml"
reconciled_config_file="$repository_root/declarative-postgres-backrest/tests/reconciled-config.yaml"
restore_config_file="$repository_root/declarative-postgres-backrest/tests/restore-config.yaml"
invalid_config_file="$repository_root/declarative-postgres-backrest/tests/invalid-owner-config.yaml"
suffix="$(date +%s)"
network="declarative-postgres-backrest-test-${suffix}"
backup_minio="declarative-postgres-backrest-backup-minio-${suffix}"
restore_minio="declarative-postgres-backrest-restore-minio-${suffix}"
primary="declarative-postgres-backrest-primary-${suffix}"
restore="declarative-postgres-backrest-restore-${suffix}"
invalid="declarative-postgres-backrest-invalid-${suffix}"
primary_volume="declarative-postgres-backrest-primary-data-${suffix}"
restore_volume="declarative-postgres-backrest-restore-data-${suffix}"
mc_volume="declarative-postgres-backrest-mc-${suffix}"

cleanup() {
	docker rm -f "$invalid" "$restore" "$primary" "$backup_minio" "$restore_minio" >/dev/null 2>&1 || true
    docker volume rm "$primary_volume" "$restore_volume" "$mc_volume" >/dev/null 2>&1 || true
    docker network rm "$network" >/dev/null 2>&1 || true
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
trap cleanup EXIT

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

docker network create "$network" >/dev/null
docker volume create "$primary_volume" >/dev/null
docker volume create "$restore_volume" >/dev/null
docker volume create "$mc_volume" >/dev/null

docker run -d --name "$invalid" --volume "$invalid_config_file":/etc/postgres-supervisor/config.yaml:ro "$image" >/dev/null
invalid_exit_code="$(docker wait "$invalid")"
test "$invalid_exit_code" -ne 0
docker logs "$invalid" | grep -q 'cannot manage grants on schema'

docker run -d --name "$backup_minio" --network "$network" --network-alias declarative-postgres-backrest-backup-minio \
    -e MINIO_ROOT_USER=integration-key \
    -e MINIO_ROOT_PASSWORD=integration-secret \
    minio/minio:RELEASE.2025-04-22T22-12-26Z server /data >/dev/null
docker run -d --name "$restore_minio" --network "$network" --network-alias declarative-postgres-backrest-restore-minio \
    -e MINIO_ROOT_USER=integration-key \
    -e MINIO_ROOT_PASSWORD=integration-secret \
    minio/minio:RELEASE.2025-04-22T22-12-26Z server /data >/dev/null

wait_for backup-minio docker run --rm --network "$network" --volume "$mc_volume":/root/.mc \
    minio/mc:RELEASE.2025-04-16T18-13-26Z alias set backup http://"$backup_minio":9000 integration-key integration-secret
docker run --rm --network "$network" --volume "$mc_volume":/root/.mc \
    minio/mc:RELEASE.2025-04-16T18-13-26Z mb backup/postgres-backups >/dev/null
wait_for restore-minio docker run --rm --network "$network" --volume "$mc_volume":/root/.mc \
    minio/mc:RELEASE.2025-04-16T18-13-26Z alias set restore http://"$restore_minio":9000 integration-key integration-secret
docker run --rm --network "$network" --volume "$mc_volume":/root/.mc \
    minio/mc:RELEASE.2025-04-16T18-13-26Z mb restore/postgres-backups >/dev/null

backup_env=(
    -e INTEGRATION_POSTGRES_PASSWORD=postgres-password
    -e INTEGRATION_OWNER_PASSWORD=owner-password
    -e INTEGRATION_READER_ONE_PASSWORD=reader-one-password
    -e INTEGRATION_READER_TWO_PASSWORD=reader-two-password
    -e INTEGRATION_WRITER_PASSWORD=writer-password
    -e INTEGRATION_S3_ACCESS_KEY=integration-key
    -e INTEGRATION_S3_SECRET_KEY=integration-secret
)

docker run -d --name "$primary" --network "$network" --volume "$primary_volume":/var/lib/postgresql \
    --volume "$config_file":/etc/postgres-supervisor/config.yaml:ro \
    "${backup_env[@]}" "$image" >/dev/null

if ! wait_for primary-ready docker exec "$primary" pg_isready -q -U integration_admin -d app; then
    docker logs "$primary"
    exit 1
fi
if ! wait_for supervisor-ready docker exec "$primary" postgres-supervisor healthcheck; then
    docker logs "$primary"
    exit 1
fi
wait_for initial-backup docker exec -u postgres "$primary" bash -c 'pgbackrest --stanza=integration --output=json info | grep -q '"'"'"label"'"'"''
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
docker rm "$primary" >/dev/null
rotated_backup_env=(
    -e INTEGRATION_POSTGRES_PASSWORD=rotated-postgres-password
    -e INTEGRATION_OWNER_PASSWORD=owner-password
    -e INTEGRATION_READER_ONE_PASSWORD=rotated-reader-one-password
    -e INTEGRATION_READER_TWO_PASSWORD=reader-two-password
    -e INTEGRATION_WRITER_PASSWORD=rotated-writer-password
    -e INTEGRATION_AUDIT_READER_PASSWORD=audit-reader-password
    -e INTEGRATION_S3_ACCESS_KEY=integration-key
    -e INTEGRATION_S3_SECRET_KEY=integration-secret
)
docker run -d --name "$primary" --network "$network" --volume "$primary_volume":/var/lib/postgresql \
    --volume "$reconciled_config_file":/etc/postgres-supervisor/config.yaml:ro \
    "${rotated_backup_env[@]}" "$image" >/dev/null
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

docker exec "$primary" gosu postgres pgbackrest --stanza=integration --type=diff backup >/dev/null
docker stop "$primary" >/dev/null
docker rm "$primary" >/dev/null
docker run --rm --network "$network" --volume "$mc_volume":/root/.mc \
    minio/mc:RELEASE.2025-04-16T18-13-26Z mirror backup/postgres-backups restore/postgres-backups >/dev/null

restore_env=(
    -e PGBACKREST_RESTORE_ENABLED=true
    -e INTEGRATION_POSTGRES_PASSWORD=rotated-postgres-password
    -e INTEGRATION_S3_ACCESS_KEY=integration-key
    -e INTEGRATION_S3_SECRET_KEY=integration-secret
)

docker run -d --name "$restore" --network "$network" --volume "$restore_volume":/var/lib/postgresql \
    --volume "$restore_config_file":/etc/postgres-supervisor/config.yaml:ro \
    "${restore_env[@]}" "$image" >/dev/null

if ! wait_for restore-ready docker exec "$restore" pg_isready -q -U integration_admin -d app; then
    docker logs "$restore"
    exit 1
fi
docker exec "$restore" psql -U integration_admin -d app -tAc 'select value from app.records where id = 1' | grep -qx present
docker exec "$restore" psql -U integration_admin -d app -tAc 'select value from app.records where id = 2' | grep -qx updated
docker exec -e PGPASSWORD=audit-reader-password "$restore" psql -h 127.0.0.1 -U audit_reader -d app -tAc 'select value from app.reconciled_records where id = 1' | grep -qx reconciled
