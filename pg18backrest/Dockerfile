FROM golang:1.25.0-bookworm AS supervisor-build

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

ENV POSTGRES_SUPERVISOR_CONFIG=/etc/postgres-supervisor/config.yaml \
    POSTGRES_SUPERVISOR_HEALTH_ADDR=:8081

ENTRYPOINT ["postgres-supervisor"]

HEALTHCHECK --interval=30s --timeout=10s --start-period=10m --retries=3 CMD ["postgres-supervisor", "healthcheck"]

EXPOSE 5432 8081
CMD ["postgres"]
