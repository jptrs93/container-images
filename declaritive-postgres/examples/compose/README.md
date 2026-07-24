# Docker Compose Example

This directory is a local Docker Compose example, not the required deployment model for `declaritive-postgres`.

Copy `.env.example` to `.env`, replace its placeholders, then run:

```bash
docker compose -f compose.yaml up -d
```

The example mounts the repository's `config.example.yaml`. Create a deployment-specific YAML file and change the mount before using Compose beyond a local smoke test. Do not store production secret values in `.env`.
