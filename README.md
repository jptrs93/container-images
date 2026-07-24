# container-images

Container images maintained by [jptrs93](https://github.com/jptrs93).

| Image | Description |
| --- | --- |
| [`declarative-postgres-backrest`](declarative-postgres-backrest/) | PostgreSQL 18 with optional pgBackRest backups to an S3-compatible object store. |
| [`declaritive-postgres`](declaritive-postgres/) | PostgreSQL 18 with declarative initialization and reconciliation, without bundled backup tooling. |

Images are published to GitHub Container Registry under `ghcr.io/jptrs93`.

Release Git tags are scoped to the image directory. For example, `declarative-postgres-backrest/18.4_2.58.0_v1` publishes the `18.4_2.58.0_v1` container tag for that image, while `declaritive-postgres/18.4_v1` publishes the `18.4_v1` container tag for the image without pgBackRest.
