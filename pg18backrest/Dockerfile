FROM golang:1.26.5-bookworm AS supervisor-build

WORKDIR /src

COPY pg18backrest/go.mod pg18backrest/go.sum ./
RUN go mod download

COPY pg18backrest/cmd ./cmd
COPY pg18backrest/internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /postgres-supervisor ./cmd/postgres-supervisor

FROM postgres:18.4-bookworm

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends pgbackrest; \
    rm -rf /var/lib/apt/lists/*; \
    localedef -i en_GB -c -f UTF-8 en_GB.UTF-8

COPY --from=supervisor-build /postgres-supervisor /usr/local/bin/postgres-supervisor

ENV PGBACKREST_ENABLED=false \
    PGBACKREST_S3_REGION=us-east-1 \
    PGBACKREST_S3_URI_STYLE=path \
    PGBACKREST_S3_VERIFY_TLS=true \
    PGBACKREST_FULL_SCHEDULE='0 2 * * 0' \
    PGBACKREST_DIFF_SCHEDULE='0 2 * * 1-6' \
    PGBACKREST_CHECK_SCHEDULE='*/5 * * * *' \
    PGBACKREST_INITIAL_BACKUP=true \
    PGBACKREST_RETENTION_FULL=2 \
    PGBACKREST_RETENTION_ARCHIVE=2 \
    PGBACKREST_PROCESS_MAX=4 \
    PGBACKREST_ARCHIVE_PUSH_QUEUE_MAX=1GiB \
    PGBACKREST_ARCHIVE_TIMEOUT=60 \
    POSTGRES_SUPERVISOR_CONFIG=/etc/postgres-supervisor/config.yaml \
    POSTGRES_SUPERVISOR_HEALTH_ADDR=:8081 \
    TZ=UTC

ENTRYPOINT ["postgres-supervisor"]

HEALTHCHECK --interval=30s --timeout=10s --start-period=10m --retries=3 CMD ["postgres-supervisor", "healthcheck"]

EXPOSE 5432 8081
CMD ["postgres"]
