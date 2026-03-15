# R2 Deployment

WP Composer deploys built repository artifacts to Cloudflare R2 for serving via CDN. Builds are generated locally (fast filesystem I/O), then synced to R2 on deploy.

## Shared Deploy Model

Both `p/` and `p2/` files are stored at shared top-level prefixes. Content-addressed `p/` files (containing `$`) are immutable and uploaded once ever. Mutable `p2/` files are overwritten in place when packages change. Only per-release index files (`packages.json`, `manifest.json`) go under a release prefix. The root `packages.json` acts as an atomic pointer identifying the current build.

```
packages.json                                        ← atomic pointer (build-id + URLs)
p/wp-plugin/akismet$abc123.json                      ← shared, uploaded once ever (immutable)
p/providers-week$def456.json                         ← shared, uploaded once ever (immutable)
p2/wp-plugin/akismet.json                            ← shared, overwritten when changed
releases/20260314-150405/packages.json               ← per-release snapshot (immutable)
releases/20260314-150405/manifest.json               ← per-release (immutable)
```

The deploy diffs the current build against the previous build locally. Content-addressed `p/` files present in both builds are skipped (zero R2 ops). Mutable `p2/` files are byte-compared and only uploaded if changed. This reduces R2 operations from ~140k to only the number of changed packages per build.

On the first deploy with this layout (upgrading from the old release-prefixed model, or no previous build), all files are uploaded. The deploy detects this by reading the current root `packages.json` from R2 — if `providers-url` still points into `releases/`, the shared prefix doesn't exist yet and the local diff is bypassed.

## Prerequisites

1. A Cloudflare account with R2 enabled.
2. An R2 bucket created for the repository (e.g., `wp-composer-repo`).
3. An R2 API token with read/write access to the bucket.
4. AWS CLI v2 installed (for manual operations and debugging).

## R2 Bucket Setup

### Create the bucket

In the Cloudflare dashboard: **R2 > Create bucket**. Pick a name (e.g., `wp-composer-repo`), choose a location hint close to your server.

### Create API credentials

**R2 > Manage R2 API Tokens > Create API Token**:
- Permission: **Object Read & Write**
- Scope: the specific bucket

Save the **Access Key ID** and **Secret Access Key**.

### Connect a custom domain (recommended)

**R2 > your bucket > Settings > Custom Domains > Connect Domain**. This gives you a URL like `https://repo.wp-composer.com` backed by Cloudflare's CDN.

Without a custom domain, R2 provides a `.r2.dev` URL, but it has rate limits and no caching.

## Environment Configuration

```env
# R2 credentials
R2_ACCESS_KEY_ID=your-access-key-id
R2_SECRET_ACCESS_KEY=your-secret-access-key
R2_BUCKET=wp-composer-repo
R2_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com

# Enable R2 deploy
WP_COMPOSER_DEPLOY_R2=true
```

Find your account ID in the Cloudflare dashboard under **R2 > Overview**.

## How Deploy Works

When deploying to R2 (`wpcomposer deploy --to-r2`):

1. Validates the build (packages.json and manifest.json must exist).
2. Diffs all files against the previous build directory. Content-addressed `p/` files present in both builds are skipped. Mutable `p2/` files are byte-compared and only uploaded if changed. Per-release index files (`packages.json`, `manifest.json`) are always uploaded under `releases/<build-id>/`. Each upload retries up to 3 times with exponential backoff.
3. Rewrites `packages.json` with the build ID embedded. All URL templates point at shared top-level prefixes.
4. Uploads the rewritten `packages.json` as the root — the atomic switch.
5. Promotes the local build symlink (for rollback capability).

If R2 sync fails, the local symlink is **not** updated — the previous build remains promoted.

Files are uploaded with `Content-Type: application/json`.

## CDN Cache Headers

When using a Cloudflare custom domain on the R2 bucket, cache behavior is controlled by the `Cache-Control` headers set during upload:

| Path pattern | Cache-Control | Rationale |
|---|---|---|
| `packages.json` (root) | `max-age=300` | Atomic pointer, only mutable object |
| `releases/*` (everything) | `max-age=31536000, immutable` | Entire release prefix is immutable |
| `p/*$hash.json` (shared) | `max-age=31536000, immutable` | Content-addressed, never changes |
| `p2/*.json` (shared) | `max-age=300` | Mutable, overwritten on package changes |

## URL Requirements

The generated root `packages.json` on R2 contains unprefixed URLs pointing at shared top-level prefixes:

- `metadata-url`: `/p2/%package%.json` (shared, overwritten in place)
- `providers-url`: `/p/%package%$%hash%.json` (shared, content-addressed)
- `provider-includes` keys: `p/providers-*$hash.json` (shared, content-addressed)
- `build-id`: the current build ID (used by cleanup to identify the live release)
- `notify-batch`: absolute URL pointing to the **app domain** (not R2, not rewritten)

## AWS CLI Setup (Manual Operations)

Configure a named profile for R2:

```bash
aws configure --profile r2
```

Enter:
- **Access Key ID**: your R2 access key
- **Secret Access Key**: your R2 secret key
- **Default region**: `auto`
- **Default output format**: `json`

### Verify access

```bash
aws s3 ls s3://wp-composer-repo/ --profile r2 --endpoint-url https://<account-id>.r2.cloudflarestorage.com
```

### List bucket contents

```bash
aws s3 ls s3://wp-composer-repo/releases/ --profile r2 --endpoint-url https://<account-id>.r2.cloudflarestorage.com
```

### Cleanup stale R2 releases

The `wpcomposer pipeline` command automatically cleans up old R2 releases after each successful deploy, keeping the live release + 5 most recent + releases within a 24-hour grace window. Cleanup is best-effort — failures are logged as warnings but do not fail the pipeline. If cleanup errors persist, run manual cleanup to investigate.

For manual cleanup:

```bash
# Remove R2 releases beyond retention (keeps live + 5 most recent + within grace period)
wpcomposer deploy --cleanup --r2-cleanup

# Shorter grace period (default 24 hours)
wpcomposer deploy --cleanup --r2-cleanup --grace-hours 6
```

`--r2-cleanup` is required — plain `--cleanup` only removes local build directories. The cleanup reads R2 state directly (no local filesystem dependency), identifies release prefixes, and deletes those outside the keep set. It also deletes legacy flat files (anything not under `releases/`, `p/`, or `p2/`, except root `packages.json`). Shared `p/` and `p2/` files are preserved — GC of orphaned shared files is deferred to a future release.

The keep set is: live release (from root `packages.json`) + releases within `--grace-hours` + top `--retain` most recent. The retain count has a hard minimum of 5 — even if `--retain` is set lower, at least 5 recent releases are always preserved.

## Rollback

Rollback is a regular incremental deploy of the target build — it diffs the target build's files against the currently deployed build and uploads only changed `p/` and `p2/` files:

```bash
wpcomposer deploy --rollback --to-r2
wpcomposer deploy --rollback=20260313-130000 --to-r2
```

Rollback takes roughly the same time as a normal deploy (proportional to the number of changed files).

## Local-Only Mode

When `WP_COMPOSER_DEPLOY_R2` is unset or `false`, deploy only updates the local `current` symlink. Use this for development or when serving directly from the local filesystem.

## Monitoring

After deploy, verify the bucket:

```bash
# Check root packages.json has build-id and unprefixed URLs
curl -s https://repo.wp-composer.com/packages.json | jq '.["build-id"], .["metadata-url"]'

# Check a specific package at the shared p2/ prefix
curl -s https://repo.wp-composer.com/p2/wp-plugin/akismet.json | head -c 200
```
