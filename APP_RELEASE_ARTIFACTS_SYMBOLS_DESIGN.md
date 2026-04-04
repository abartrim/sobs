# App/Release Artifact & Symbols Design

## Goal

Move stack/symbol resolution from ad-hoc local path lookup to an app-centric, release-aware model:

- Configure apps explicitly.
- Register immutable releases (version, commit, build metadata).
- Upload/refer source maps and symbol artifacts per release.
- Resolve errors with deterministic app+release matching.

This should support JS source maps now and extend to native symbols later.

## Why This Model

Current source-map support is useful for local/dev, but production workflows need:

- Determinism: exact artifact set for a given release.
- Security: avoid broad runtime repo access.
- Performance: local/object-store fetch + cache instead of git/API traversals.
- Auditability: clear app/release ownership and retention.

## Scope

### In scope

- App registry
- Release registry
- Artifact registry (source maps and symbols)
- Error resolution using app/release metadata
- CI upload flow

### Out of scope (first phase)

- Automatic source retrieval from arbitrary repo branches
- Full symbolication for every language/runtime
- Build-system-specific plugins (can come later)

## Data Model

### App

Represents a product/service emitting telemetry.

Fields:

- id (uuid)
- name (string, unique)
- slug (string, unique)
- owner_team (string)
- repo_url (optional)
- default_environment (optional)
- created_at
- updated_at
- enabled (bool)

### Release

Immutable build/release descriptor for an app.

Fields:

- id (uuid)
- app_id (fk)
- version (string)
- commit_sha (string)
- build_id (optional)
- environment (optional)
- released_at
- metadata (json)

Suggested uniqueness:

- (app_id, version, commit_sha, environment)

### Artifact

Binary or index used for deobfuscation/symbolication.

Fields:

- id (uuid)
- release_id (fk)
- artifact_type (enum):
  - js_sourcemap
  - js_bundle_manifest
  - native_symbol
  - debug_info
- name (string)
- content_type
- size
- storage_url or local_path
- checksum_sha256
- platform (optional)
- architecture (optional)
- uploaded_at
- metadata (json)

For JS source maps, metadata may include:

- bundle_url_prefix
- bundle_filename
- source_map_filename

## Event Contract Additions

To make matching deterministic, browser/runtime events should carry:

- appName
- appVersion
- commitSha (recommended)
- environment
- buildId (optional)

If missing, fallback logic can still attempt weaker matching, but strong matching is preferred.

## API Proposal

### App APIs

- POST /v1/apps
- GET /v1/apps
- GET /v1/apps/{app_id}
- PATCH /v1/apps/{app_id}
- DELETE /v1/apps/{app_id} (soft-delete preferred)

### Release APIs

- POST /v1/apps/{app_id}/releases
- GET /v1/apps/{app_id}/releases
- GET /v1/releases/{release_id}

### Artifact APIs

- POST /v1/releases/{release_id}/artifacts (signed upload or direct upload)
- GET /v1/releases/{release_id}/artifacts
- GET /v1/artifacts/{artifact_id}

### Resolution APIs (optional explicit endpoints)

- POST /v1/errors/resolve-stack
  - body includes stack + app/release fields
  - returns mapped frames and confidence

## Resolution Strategy

Given an incoming stack frame and event metadata:

1. Identify app
   - appName exact match preferred
2. Identify release
   - app + version + commitSha + environment
   - fallback: app + version
3. Select artifacts
   - JS source maps matching bundle file/path
4. Remap frames
   - annotate mapped frames
   - preserve original frame for audit

Confidence scoring (optional):

- high: app+version+commit exact
- medium: app+version only
- low: heuristic fallback

## Storage

Two viable modes:

- Local disk (single-node/simple mode)
- Object storage (S3-compatible) for production

Keep metadata in chDB tables and artifact payloads in storage.

## Security Model

### Upload auth

- Server-side API key/Bearer for CI release registration.
- Optional signed upload for artifact bytes.

### Least privilege

- No broad read access to VCS from runtime process.
- CI pushes only required artifacts.

### Integrity

- checksum verification at upload
- immutable release artifacts (no overwrite)

## CI/CD Flow

1. Build pipeline compiles app.
2. Generate source maps/symbols.
3. Call SOBS API:
   - ensure app exists
   - create release
   - upload artifacts
4. Deploy app with release metadata env vars:
   - app name
   - version
   - commit
5. Runtime telemetry includes release metadata.
6. SOBS resolves stacks using matching release artifacts.

## Compatibility with Current Implementation

Current environment-driven mapping can remain as fallback:

- SOBS_SOURCE_MAP_ENABLE
- SOBS_SOURCE_MAP_DIR

Proposed behavior:

- If app/release artifacts exist, use them first.
- Otherwise fallback to directory-based remap.

## Rollout Plan

### Phase 1

- Add app/release/artifact metadata tables.
- Add CRUD APIs for app/release/artifact metadata.
- Keep current local sourcemap path remapping active.

### Phase 2

- Add artifact upload endpoints and resolver lookup by app/release.
- Use event metadata (app/version/commit) in resolution path.

### Phase 3

- Add CI helper scripts/examples for automatic registration.
- Add UI pages for app/release/artifact management.

### Phase 4

- Extend beyond JS source maps (native symbols, other runtimes).

## Open Questions

- Should release uniqueness include environment or be environment-agnostic?
- Do we allow replacing artifacts within a release, or enforce strict immutability?
- What retention defaults should apply to old releases/artifacts?
- Do we need multi-tenant isolation in this phase?

## Recommendation

Use app/release/artifact registry as the primary production mode, keep local directory remapping as a backwards-compatible fallback, and ship CI-first onboarding so teams can adopt with minimal manual steps.
