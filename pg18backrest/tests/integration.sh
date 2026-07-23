#!/usr/bin/env bash
set -Eeuo pipefail

image="${1:-pg18backrest:test}"
repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
config_file="$repository_root/pg18backrest/tests/config.yaml"
restore_config_file="$repository_root/pg18backrest/tests/restore-config.yaml"
suffix="$(date +%s)"
network="pg18backrest-test-${suffix}"
minio="pg18backrest-minio-${suffix}"
primary="pg18backrest-primary-${suffix}"
restore="pg18backrest-restore-${suffix}"
primary_volume="pg18backrest-primary-data-${suffix}"
restore_volume="pg18backrest-restore-data-${suffix}"
mc_volume="pg18backrest-mc-${suffix}"

cleanup() {
    docker rm -f "$restore" "$primary" "$minio" >/dev/null 2>&1 || true
    docker volume rm "$primary_volume" "$restore_volume" "$mc_volume" >/dev/null 2>&1 || true
    docker network rm "$network" >/dev/null 2>&1 || true
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

docker run -d --name "$minio" --network "$network" --network-alias pg18backrest-minio \
    -e MINIO_ROOT_USER=integration-key \
    -e MINIO_ROOT_PASSWORD=integration-secret \
    minio/minio:RELEASE.2025-04-22T22-12-26Z server /data >/dev/null

wait_for minio docker run --rm --network "$network" --volume "$mc_volume":/root/.mc \
    minio/mc:RELEASE.2025-04-16T18-13-26Z alias set local http://"$minio":9000 integration-key integration-secret
docker run --rm --network "$network" --volume "$mc_volume":/root/.mc \
    minio/mc:RELEASE.2025-04-16T18-13-26Z mb local/postgres-backups >/dev/null

backup_env=(
    -e INTEGRATION_POSTGRES_PASSWORD=postgres-password
    -e INTEGRATION_APP_PASSWORD=app-password
    -e INTEGRATION_S3_ACCESS_KEY=integration-key
    -e INTEGRATION_S3_SECRET_KEY=integration-secret
)

docker run -d --name "$primary" --network "$network" --volume "$primary_volume":/var/lib/postgresql \
    --volume "$config_file":/etc/postgres-supervisor/config.yaml:ro \
    "${backup_env[@]}" "$image" >/dev/null

wait_for primary-ready docker exec "$primary" pg_isready -q -U integration_admin -d app
if ! wait_for supervisor-ready docker exec "$primary" postgres-supervisor healthcheck; then
    docker logs "$primary"
    exit 1
fi
wait_for initial-backup docker exec -u postgres "$primary" bash -c 'pgbackrest --stanza=integration --output=json info | grep -q '"'"'"label"'"'"''
docker exec "$primary" psql -U integration_admin -d app -tAc 'show max_connections' | grep -qx 50
docker exec "$primary" psql -U integration_admin -d app -tAc "select has_schema_privilege('app_user', 'public', 'USAGE')" | grep -qx t
docker exec "$primary" psql -U integration_admin -d app -tAc "select exists(select 1 from pg_namespace where nspname = 'staging')" | grep -qx t
docker exec "$primary" psql -U integration_admin -d app -tAc "select exists(select 1 from pg_extension where extname = 'pgcrypto')" | grep -qx t
docker exec "$primary" bash -c "PGPASSWORD=app-password psql -h 127.0.0.1 -U app_user -d app -tAc 'select 1'" | grep -qx 1

docker stop "$primary" >/dev/null
docker rm "$primary" >/dev/null
rotated_backup_env=(
    -e INTEGRATION_POSTGRES_PASSWORD=rotated-postgres-password
    -e INTEGRATION_APP_PASSWORD=app-password
    -e INTEGRATION_S3_ACCESS_KEY=integration-key
    -e INTEGRATION_S3_SECRET_KEY=integration-secret
)
docker run -d --name "$primary" --network "$network" --volume "$primary_volume":/var/lib/postgresql \
    --volume "$config_file":/etc/postgres-supervisor/config.yaml:ro \
    "${rotated_backup_env[@]}" "$image" >/dev/null
wait_for primary-ready-after-rotation docker exec "$primary" pg_isready -q -U integration_admin -d app
if ! wait_for supervisor-ready-after-rotation docker exec "$primary" postgres-supervisor healthcheck; then
    docker logs "$primary"
    exit 1
fi
if docker exec "$primary" bash -c "PGPASSWORD=postgres-password psql -h 127.0.0.1 -U integration_admin -d app -tAc 'select 1'" >/dev/null 2>&1; then
    printf 'old superuser password still works after rotation\n' >&2
    exit 1
fi
docker exec "$primary" bash -c "PGPASSWORD=rotated-postgres-password psql -h 127.0.0.1 -U integration_admin -d app -tAc 'select 1'" | grep -qx 1

docker exec "$primary" psql -U integration_admin -d app -c "create table restore_check (value text not null); insert into restore_check values ('present');" >/dev/null
docker exec "$primary" gosu postgres pgbackrest --stanza=integration --type=diff backup >/dev/null
docker stop "$primary" >/dev/null
docker rm "$primary" >/dev/null

restore_env=(
    -e PGBACKREST_RESTORE_ENABLED=true
    -e INTEGRATION_POSTGRES_PASSWORD=rotated-postgres-password
    -e INTEGRATION_S3_ACCESS_KEY=integration-key
    -e INTEGRATION_S3_SECRET_KEY=integration-secret
)

docker run -d --name "$restore" --network "$network" --volume "$restore_volume":/var/lib/postgresql \
    --volume "$restore_config_file":/etc/postgres-supervisor/config.yaml:ro \
    "${restore_env[@]}" "$image" >/dev/null

wait_for restore-ready docker exec "$restore" pg_isready -q -U integration_admin -d app
docker exec "$restore" psql -U integration_admin -d app -tAc 'select value from restore_check' | grep -qx present
