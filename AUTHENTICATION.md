# Authentication Setup

SOBS supports two independent auth areas:

- Ingest API auth for `/v1/*` via `SOBS_API_KEY`
- Web UI auth for `/`, `/logs`, `/errors`, `/traces`, `/rum`, `/ai`, `/tail`

Web UI auth mode is exclusive. Configure exactly one mode:

- none
- basic
- external

If the configuration is mixed or incomplete, SOBS returns `500` with:

`{"error": "Server auth misconfiguration"}`

## 1) Ingest API auth (`SOBS_API_KEY`)

Set `SOBS_API_KEY` to require an API key for ingest endpoints.

Example environment:

    SOBS_API_KEY=my-ingest-key

Send data with header:

    X-API-Key: my-ingest-key

Example request:

    curl -X POST http://localhost:44317/v1/logs \
      -H "Content-Type: application/json" \
      -H "X-API-Key: my-ingest-key" \
      -d '{}'

## 2) Web UI mode: none (no UI auth)

Leave all UI auth variables empty:

    SOBS_BASIC_AUTH_USERNAME=
    SOBS_BASIC_AUTH_PASSWORD=
    SOBS_EXTERNAL_AUTH_URL=

Result:

- Web UI routes are open

## 3) Web UI mode: basic

Set both Basic credentials, and do not set external auth:

    SOBS_BASIC_AUTH_USERNAME=admin
    SOBS_BASIC_AUTH_PASSWORD=secret
    SOBS_EXTERNAL_AUTH_URL=

Result:

- Web UI requires `Authorization: Basic ...`
- Unauthorized responses include `WWW-Authenticate: Basic realm="SOBS"`

Example (generates Basic header using base64):

    curl -i http://localhost:44317/ \
      -H "Authorization: Basic $(printf 'admin:secret' | base64)"

## 4) Web UI mode: external

Set external validator URL only:

    SOBS_EXTERNAL_AUTH_URL=http://auth-service
    SOBS_BASIC_AUTH_USERNAME=
    SOBS_BASIC_AUTH_PASSWORD=

SOBS validates bearer tokens by POSTing to:

`{SOBS_EXTERNAL_AUTH_URL}/internal/auth/validate`

It forwards the incoming `Authorization` header and accepts the request only when validation returns HTTP `200`.

Result:

- Web UI requires `Authorization: Bearer ...`
- Unauthorized responses include `WWW-Authenticate: Bearer realm="SOBS"`

Example request:

    curl -i http://localhost:44317/ \
      -H "Authorization: Bearer eyJhbGciOi..."

### Same-origin session-cookie fallback

When SOBS UI is served under `/sobs` on the same domain as a management UI, browsers include the management `session` cookie but typically do **not** send an explicit `Authorization: Bearer ...` header.

To support this deployment model, external auth mode automatically falls back to the `session` cookie when no `Authorization` header is present:

1. If `Authorization: Bearer ...` is present, it is forwarded to the external validator as usual.
2. If no Bearer header is present and a `session` cookie exists, SOBS synthesizes `Authorization: Bearer <session_cookie_value>` and forwards that to the external validator.
3. If neither a Bearer header nor a `session` cookie is present, SOBS returns `401` with `WWW-Authenticate: Bearer realm="SOBS"`.

This allows users already authenticated in the management UI to access `/sobs` routes without any extra configuration or manual bearer injection.

## 5) Invalid UI auth configurations

These are invalid and will return `500` on Web UI routes:

- Only one of `SOBS_BASIC_AUTH_USERNAME` or `SOBS_BASIC_AUTH_PASSWORD` is set
- Basic credentials are set and `SOBS_EXTERNAL_AUTH_URL` is also set

## Docker Compose example

Basic mode:

    services:
      sobs:
        image: ghcr.io/abartrim/sobs:latest
        ports:
          - "44317:4317"
        environment:
          - SOBS_API_KEY=my-ingest-key
          - SOBS_BASIC_AUTH_USERNAME=admin
          - SOBS_BASIC_AUTH_PASSWORD=secret

External mode:

    services:
      sobs:
        image: ghcr.io/abartrim/sobs:latest
        ports:
          - "44317:4317"
        environment:
          - SOBS_API_KEY=my-ingest-key
          - SOBS_EXTERNAL_AUTH_URL=http://auth-service

## Quick copy/paste setups

### Local dev (.env style)

No UI auth:

    SOBS_API_KEY=my-ingest-key
    SOBS_BASIC_AUTH_USERNAME=
    SOBS_BASIC_AUTH_PASSWORD=
    SOBS_EXTERNAL_AUTH_URL=

Basic UI auth:

    SOBS_API_KEY=my-ingest-key
    SOBS_BASIC_AUTH_USERNAME=admin
    SOBS_BASIC_AUTH_PASSWORD=secret
    SOBS_EXTERNAL_AUTH_URL=

External UI auth:

    SOBS_API_KEY=my-ingest-key
    SOBS_BASIC_AUTH_USERNAME=
    SOBS_BASIC_AUTH_PASSWORD=
    SOBS_EXTERNAL_AUTH_URL=http://auth-service

### Docker Compose environment block

Basic UI auth:

    environment:
      - SOBS_API_KEY=my-ingest-key
      - SOBS_BASIC_AUTH_USERNAME=admin
      - SOBS_BASIC_AUTH_PASSWORD=secret

External UI auth:

    environment:
      - SOBS_API_KEY=my-ingest-key
      - SOBS_EXTERNAL_AUTH_URL=http://auth-service

### Kubernetes quick commands

Basic UI auth:

    kubectl create secret generic sobs-auth \
      --from-literal=SOBS_API_KEY=my-ingest-key \
      --from-literal=SOBS_BASIC_AUTH_USERNAME=admin \
      --from-literal=SOBS_BASIC_AUTH_PASSWORD=secret

External UI auth:

    kubectl create secret generic sobs-auth \
      --from-literal=SOBS_API_KEY=my-ingest-key \
      --from-literal=SOBS_EXTERNAL_AUTH_URL=http://auth-service

Then reference the secret in your Deployment environment (`envFrom.secretRef` or per-key `env.valueFrom.secretKeyRef`).
