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

## Workflow

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
