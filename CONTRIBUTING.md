# Contributing

## Branching Policy (Required)

- Do not commit directly to `main`.
- Do not push directly to `main`.
- All work must use this flow: Issue -> new branch -> pull request -> review -> merge.
- Use branch names like `issue-<number>-<short-description>`.
- Every PR should reference its issue and include test/validation notes.

## Local Setup

SOBS ships as a **Go** server. Development happens in `go/` plus the shared `templates/` and
`static/`; `scripts/` holds a handful of stdlib-only Python ops/release helpers.

### Go server (primary)

```bash
cd go
go build ./...     # build everything
go vet ./...       # vet
gofmt -l .         # formatting gate (must print nothing)
go test ./...      # unit tests (chdb-backed tests need CHDB_LIB_PATH -> pinned libchdb, go/CHDB_PIN.md)

# Run locally on :44317
go build -o sobs ./cmd/sobs && SOBS_DATA_DIR=../data ./sobs
```

Live-reload dev loop — rebuilds and restarts the Go server on every save, with an example-traffic
sidecar so the UI populates:

```bash
docker compose -f docker-compose.yml -f docker-compose.dev.yml up
```

### Golden-corpus regression suite

The Go-native successor to the (now-retired) Python-parity byte-diff: boots the compiled `sobs`
binary against a frozen corpus of golden HTTP responses and byte-diffs every route.

```bash
cd go
CHDB_LIB_PATH=/path/to/libchdb.so go test -tags chdb -run TestGoldenCorpus ./goldenreplay/...
```

See [go/README.md](go/README.md) and [go/CHDB_PIN.md](go/CHDB_PIN.md) for the pinned native
chdb library this needs.

### Coverage floor

CI enforces a minimum combined statement-coverage floor (unit tests + golden-corpus replay,
generated OTLP protobuf excluded) — see `go/COVERAGE_FLOOR` and `go/coverage_gate.sh`. A PR that
drops coverage below the floor fails the build; raise `go/COVERAGE_FLOOR` in the same PR that
adds meaningful new coverage to lock in the gain. To reproduce locally:

```bash
cd go
export GOCOVERDIR=$(mktemp -d)
go test -cover -covermode=atomic ./... -args -test.gocoverdir="$GOCOVERDIR"
SOBS_GOCOVER=1 CHDB_LIB_PATH=/path/to/libchdb.so go test -tags chdb -run TestGoldenCorpus ./goldenreplay/...
./coverage_gate.sh "$GOCOVERDIR"
```

## Pre-Commit Hook

This repository ships a version-controlled Git hook at `.githooks/pre-commit`.

Enable it once per clone:

```bash
git config core.hooksPath .githooks
chmod +x .githooks/pre-commit
```

On each commit, the hook runs these checks on staged Python files:

- `isort`
- `black`
- `flake8`
- `mypy`

And on staged Jinja templates in `templates/*.html`:

- `python3 scripts/run_djlint.py --reformat --lint`

The helper lints all matched templates, but only reformats/checks explicitly targeted or branch-changed templates that do not embed Jinja inside script-heavy blocks.

If formatters update files, the hook re-stages those files automatically.

## Manual Checks

Run these before opening or updating a PR:

```bash
isort scripts examples
black scripts examples
flake8 scripts examples
python3 scripts/run_djlint.py --reformat --lint templates
python3 scripts/run_djlint.py --check --lint templates
npm run lint:examples   # ESLint over examples/nodejs and examples/rum
```

On a clean tree, `--check` applies only to templates changed on the current branch. To format a specific file directly, pass the file path instead of the `templates` directory.

`examples/` holds real integration snippets (Node.js, browser RUM helpers, Python), not throwaway
samples — they're linted the same as everything else. `eslint.config.js` scopes ESLint to
`examples/nodejs` and `examples/rum` only; the shipped RUM bundle source is type-checked
separately via `npm run typecheck:rum`.

## Regenerating Docs Screenshots

There is currently no automated screenshot-capture harness (it lived in the now-retired Python
integration test suite). Help-page screenshots under `static/help/` need to be captured manually
against a running instance until a Go-native replacement exists.

## Releasing

Releases are driven by a Git tag pushed to GitHub. CI picks up any tag matching `v*`, runs the full test pipeline, then builds and publishes a multi-arch Docker image stamped with the version.

### Steps

1. **Ensure `main` is green** — all CI checks must pass before tagging.

2. **Create and push a version tag:**

   ```bash
   git tag v1.2.3
   git push origin v1.2.3
   ```

3. **Create a GitHub Release** from that tag (via the GitHub UI or CLI). The release description becomes the public changelog entry.

   ```bash
   gh release create v1.2.3 --title "v1.2.3" --notes "Release notes here"
   ```

4. **CI publishes the Go image automatically.** The `docker` job in `.github/workflows/ci.yml`
   detects `refs/tags/v*`, builds `Dockerfile.go` (gated on the `go-build-test` + `docker-go-smoke`
   jobs, so a release only ships a build that passed unit tests and the golden-corpus replay),
   passes `SOBS_BUILD_VERSION=v1.2.3` as a build arg, and pushes the multi-arch image to GHCR with
   both the version tag and a new `latest`:

   - `ghcr.io/abartrim/sobs:v1.2.3`
   - `ghcr.io/abartrim/sobs:latest`

5. **Verify** the version appears in the sidebar footer of a freshly pulled container:

   ```bash
   docker pull ghcr.io/abartrim/sobs:v1.2.3
   docker run -p 44317:4317 ghcr.io/abartrim/sobs:v1.2.3
   # Open http://localhost:44317 — sidebar footer should show "v1.2.3"
   ```

### Version format

Use [Semantic Versioning](https://semver.org/): `vMAJOR.MINOR.PATCH` (e.g. `v1.2.3`). Pre-release suffixes like `v1.2.3-beta` are supported. Images built from `main` without a tag show `dev` in the sidebar.