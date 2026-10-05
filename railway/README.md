# Railway deployment (fork: huangruoqi/sub2api)

Images are built by GitHub Actions and pushed to GHCR. Railway only pulls images and never builds anything.

```
push dev  ─► .github/workflows/image.yml ─► ghcr.io/huangruoqi/sub2api:dev  ─► Railway env "dev"
push main ─►            (same)           ─► ghcr.io/huangruoqi/sub2api:main ─► Railway env "production"
```

Every build is also tagged `sha-<short-sha>` so you can pin or roll back to an exact commit.

## Build process

`.github/workflows/image.yml` runs on every push to `main` or `dev`, and can be started by hand (**Actions → Image → Run workflow**).

1. Builds the root `Dockerfile` for `linux/amd64`. The Dockerfile has several stages:
   - **frontend:** `pnpm install` + `pnpm run build` (Vue) → `backend/internal/web/dist`
   - **backend:** `go build -tags embed` → a single static binary with the frontend embedded
   - **runtime:** Alpine + `pg_dump`/`psql` (used by the backup feature) + the binary; entrypoint `/app/sub2api`, port 8080, healthcheck `/health`
2. Passes `GOPROXY=proxy.golang.org`, because upstream defaults to `goproxy.cn`, which is slow from GitHub runners.
3. Caches layers in the GitHub Actions cache (`type=gha`). A cold build takes about 10 minutes; a backend-only change is much faster.
4. Pushes `:<branch>` and `:sha-<sha>` to `ghcr.io/huangruoqi/sub2api`.
5. If a Railway token secret is set, it runs `railway redeploy --from-source`, so Railway pulls the new image.

To build the same image locally: `docker build -t sub2api:local .`

## One-time setup

### GHCR
After the first successful workflow run, go to **GitHub → your profile → Packages → sub2api → Package settings**:
- Set visibility to **Public** (simplest), **or** keep it private and add registry credentials in Railway
  (service → Settings → Source → registry credentials; use a PAT with `read:packages`).

### Railway: dev environment
1. Project → **Settings → Environments → New Environment** → `dev`, **Duplicate** `production`.
   This copies the services and variables. Databases start **empty**.
2. In `dev`, open the **sub2api** service → **Settings → Source → Docker image**: `ghcr.io/huangruoqi/sub2api:dev`.
3. **Deploy**: healthcheck path `/health`. **Networking**: generate a domain with target port **8080**. Check that a **Volume** is mounted at `/app/data`.
4. Variables: keep the duplicated ones, then override the ones below. Adjust `Postgres` / `Redis` to your service names.

| Variable | Dev value | Note |
|---|---|---|
| `DATABASE_HOST` / `PORT` / `USER` / `PASSWORD` / `DBNAME` | `${{Postgres.PGHOST}}` / `${{Postgres.PGPORT}}` / `${{Postgres.PGUSER}}` / `${{Postgres.PGPASSWORD}}` / `${{Postgres.PGDATABASE}}` | Must point at the **dev** Postgres. |
| `REDIS_HOST` / `PORT` / `USERNAME` / `PASSWORD` | `${{Redis.REDISHOST}}` / `${{Redis.REDISPORT}}` / `${{Redis.REDISUSER}}` / `${{Redis.REDISPASSWORD}}` | Must point at the **dev** Redis. |
| `AUTO_SETUP` | `true` | First boot creates the schema and admin account. |
| `SERVER_HOST` / `SERVER_PORT` | `0.0.0.0` / `8080` | sub2api ignores Railway's `PORT`. |
| `ADMIN_EMAIL` / `ADMIN_PASSWORD` | dev-only | |
| `JWT_SECRET` / `TOTP_ENCRYPTION_KEY` | `openssl rand -hex 32` each | **Must differ from prod.** |

### Auto-redeploy (optional)
Railway does not notice when a tag is overwritten. To make pushes redeploy automatically:
1. Railway → Project Settings → **Tokens** → create a project token for `dev`, and another for `production`.
2. GitHub repo → Settings → Secrets → Actions: add `RAILWAY_TOKEN_DEV` and `RAILWAY_TOKEN_PRODUCTION`.

Without these secrets the workflow still pushes the image. You then redeploy by hand: service → **Deployments → Redeploy**.

## Trajectory archive (issue #8)

Every authenticated gateway POST is archived with its full request body and full response body
(SSE streams are kept as the raw event text). Each record also has request id, user, API key,
group, upstream account, model, status, latency and request headers, with credential headers removed. Records are batched every minute
into gzip JSONL objects in a **private Railway Bucket**:

```
trajectories/data/YYYY/MM/DD/HH/<replica>-<unixnano>-n<count>.jsonl.gz    full records, one JSON per line
trajectories/index/YYYY/MM/DD/HH/<same name>                              metadata only (no headers/bodies) + line number
trajectories/backfill/<date>/<table>.jsonl.gz                             one-off history export
```

Each index object sits next to its data object and holds the same lines, minus headers and bodies. The dashboard reads the index and
only opens a data object when you view a single request. `n<count>` in the name is the number of records, so totals
come from listing objects alone, without downloading anything.

Token usage is in the response body (the final `usage` / `message_delta` event). Billing rows are in `usage_logs`.
Websocket traffic (`/live`, realtime) is not captured.

Writes never block or fail a user request. A batch is written to the spool dir first (default
`/app/data/trajectory-spool` on the volume) and then uploaded. Failed uploads are retried on the next flush and on the next start.
If the in-memory queue is full (4096 records), records are dropped and a warning is logged.

### Setup (per environment)
1. Project canvas → **Create → Bucket**. Each environment gets its own bucket and credentials.
2. On the **sub2api** service, add these variables (`Bucket` is the bucket service's name):

| Variable | Value |
|---|---|
| `TRAJECTORY_ENABLED` | `true` |
| `TRAJECTORY_BUCKET` | `${{Bucket.BUCKET}}` (not `RAILWAY_BUCKET_NAME`) |
| `TRAJECTORY_ENDPOINT` | `${{Bucket.ENDPOINT}}` (`https://t3.storageapi.dev`) |
| `TRAJECTORY_REGION` | `${{Bucket.REGION}}` (`auto`) |
| `TRAJECTORY_ACCESS_KEY_ID` | `${{Bucket.ACCESS_KEY_ID}}` |
| `TRAJECTORY_SECRET_ACCESS_KEY` | `${{Bucket.SECRET_ACCESS_KEY}}` |
| `TRAJECTORY_FORCE_PATH_STYLE` | `false`. Set `true` only if the bucket's Credentials tab says path-style. |
| `RAILWAY_DEPLOYMENT_DRAINING_SECONDS` | `30`, so the last batch is uploaded on redeploy. It is in the spool either way. |

Optional settings: `TRAJECTORY_PREFIX` (default `trajectories/`), `TRAJECTORY_MAX_BODY_BYTES` (default 32 MiB per body;
longer bodies are stored as `{"truncated": true, "data": ...}`), `TRAJECTORY_SPOOL_DIR`.

3. Redeploy. The log line `trajectory archive enabled` confirms it is on. Misconfiguration logs
   `Trajectory archive disabled: ...` and the gateway keeps serving.

### Dashboard
Admin → **Trajectories** (`/admin/trajectories`):
- **Totals**: archived requests, gzip size, the latest day, batches waiting in the spool, dropped records, and a per-day chart.
  This comes from a cached (2 min) listing of `data/`.
- **Browse**: pick a window of up to 24h and filter by request id, session, model, user, key, account or status.
- **Group by** session (the default, one row per conversation) / model / user / key / account / group / path / status. Each group shows count, errors, average latency,
  size and time span. Click a group to drill into its requests. A session opens oldest-first, so you can read it as a conversation.
- **Detail**: a "Conversation" view (system prompt, last user turn, assistant output reassembled from SSE),
  plus the raw request, response and headers.

`session` is the client's own conversation id: Claude Code's `metadata.user_id` session, or Codex/OpenAI
`session_id` headers / `prompt_cache_key`. Requests without one are grouped under "(none)".
The dashboard reads the bucket through the sub2api service, so it works only where `TRAJECTORY_ENABLED=true`.

### Reading it with S3 tools
Use the bucket's credentials (Bucket → **Credentials** tab) with any S3 client:

```bash
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... AWS_DEFAULT_REGION=auto
aws s3 ls s3://<BUCKET>/trajectories/data/ --recursive --endpoint-url https://t3.storageapi.dev
aws s3 cp s3://<BUCKET>/trajectories/data/2026/10/04/12/<file>.jsonl.gz - --endpoint-url https://t3.storageapi.dev \
  | gunzip | jq 'select(.request_id=="...")'
```

The bucket is private (Railway has no public buckets). Only people with the credentials can read it, so keep them to ops.

### Backfill
Request/response bodies were never stored before this, so only metadata can be backfilled. `railway/backfill.sh` exports
`usage_logs` and the `ops_*` tables, one `row_to_json` per line, to `trajectories/backfill/<date>/`.
Run it from your machine against each environment you want archived. It needs `psql` and the `aws` CLI.

```bash
DATABASE_URL='<Postgres → Variables → DATABASE_PUBLIC_URL>' \
BUCKET=<BUCKET> ENDPOINT=https://t3.storageapi.dev \
AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
./railway/backfill.sh
```

### Retention
- Bucket objects are kept forever. Railway buckets have no lifecycle rules. Storage costs $0.015/GB-month, and
  egress/API calls are free. Check the bucket size after the first week.
- The `ops_*` tables are still auto-deleted after 30 days by the ops cleanup job. Run the backfill first, then
  turn cleanup off in the admin UI (**Ops Monitoring → Settings → Advanced Settings → Data Retention Policy → Enable Data Cleanup**). The DB setting overrides `OPS_CLEANUP_ENABLED`.
  If you would rather keep cleanup on, re-run the backfill before each retention window passes.
  New traffic no longer depends on these tables, because the trajectory archive is the full record.


1. Work on the `dev` branch → push → image `:dev` → dev environment redeploys.
2. Verify on the dev domain:
   - `curl https://<dev-domain>/health`
   - log in to the admin UI, add accounts, create a key, and exercise the change
   - for caching changes, run the cache test scripts against the dev domain and compare with production
3. Merge `dev` → `main` → image `:main`.
4. The first time only: switch production's service source from `weishaw/sub2api:latest` to `ghcr.io/huangruoqi/sub2api:main`.
5. **Rollback:** set the production image to a known-good `ghcr.io/huangruoqi/sub2api:sha-<sha>` and redeploy.

## Caveats

- **Don't use the same OAuth account in prod and dev.** ChatGPT/Claude refresh tokens rotate. When one
  deployment refreshes, the other's token becomes invalid and the prod account breaks. Use a separate test account in dev.
- **Migrations are forward-only** and run on startup. Rolling back the image does not roll back the schema.
  Avoid fork-only migrations: upstream numbers them sequentially (`backend/migrations/NNN_*.sql`), so they will collide.
  Take a Railway Postgres backup before promoting any build that changes the schema.
- **Cost:** the dev environment runs its own sub2api, Postgres and Redis. Enable **Serverless** (sleep when idle) on the dev
  service, or delete the environment when it's unused.
