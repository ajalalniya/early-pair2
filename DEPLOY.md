# Deploying EarlyPair

Releases deploy themselves: push a version tag and GitHub Actions tests the
code, builds two images, pushes them to GitHub Container Registry, and a runner
on your server pulls them and restarts the stack.

```
git tag v0.1.0 && git push origin v0.1.0
```

| Job | Runs on | Does |
|---|---|---|
| Backend tests | GitHub | `gofmt`, `go vet`, `go test` |
| Frontend tests | GitHub | type check, unit tests |
| Build and push images | GitHub, tags only | `early-pair2-backend` and `early-pair2-web` to `ghcr.io`, tagged with the version and `latest` |
| Deploy to server | your server, tags only | pulls the images, `docker compose up -d`, waits for `/healthz` |

Pushes to `main` and pull requests run the tests only.

## What runs on the server

`deploy/docker-compose.yml`, copied to the deploy directory on every release:

| Service | Image | Reachable from |
|---|---|---|
| `web` | nginx with the built app; forwards `/api` to `api` | the host port `WEB_PORT` (80) |
| `api` | backend, REST and WebSocket | inside the stack only |
| `indexer` | backend, pool discovery and tracking | inside the stack only |
| `postgres` | Postgres 16, data in the `pgdata` volume | inside the stack only |
| `redis` | Redis 7, pub/sub between api and indexer | inside the stack only |

Migrations run automatically when the api and indexer start.

## One-time server setup

A Linux server (Ubuntu 22.04 or 24.04 works well) with at least 2 GB of RAM.

### 1. Install Docker with the compose plugin

Skip the install if Docker is already there — the script warns about that
for good reason, and can leave `apt` half-configured when it clashes with an
existing install (`sudo dpkg --configure -a` finishes or explains it).

```
docker compose version        # any version, v2 or later, means you are set
curl -fsSL https://get.docker.com | sh    # only if the line above fails
```

### 2. Create the user the runner runs as

```
sudo adduser --disabled-password --gecos "" github
sudo usermod -aG docker github
```

### 3. Install the self-hosted runner

On GitHub: **the repository → Settings → Actions → Runners → New self-hosted
runner → Linux**. Run the commands it shows as the `github` user
(`sudo -iu github`), then install it as a service so it survives reboots:

```
cd ~/actions-runner
sudo ./svc.sh install github
sudo ./svc.sh start
```

The deploy job picks it up through the `self-hosted` label, which every runner has.

### 4. Create the settings file

The repository is private, so the template cannot be downloaded anonymously.
Copy it up from your machine instead:

```
# on your machine, from the repository root
scp deploy/.env.example you@your-server:/tmp/early-pair2.env
```

then on the server:

```
sudo mkdir -p /home/github/early-pair2
sudo mv /tmp/early-pair2.env /home/github/early-pair2/.env
sudo chown -R github:github /home/github/early-pair2
sudo chmod 600 /home/github/early-pair2/.env
sudo -iu github nano ~/early-pair2/.env
```

Fill in at least `POSTGRES_PASSWORD`, `EARLYPAIR_JWT_SECRET`,
`EARLYPAIR_AUTH_DOMAIN` and `EARLYPAIR_CORS_ORIGINS`. Generate the two
secrets with `openssl rand -hex 24` and `openssl rand -hex 32`. The deploy job
stops with an error if this file is missing, rather than starting with
defaults.

The deploy directory defaults to `/home/github/early-pair2`. To use another,
set a repository variable `DEPLOY_DIR` (Settings → Secrets and variables →
Actions → Variables).

### 5. HTTPS

Wallets expect the site over https. The stack serves plain http on
`WEB_PORT`; put a TLS proxy in front of it. With Caddy on the same server:

```
sudo apt install -y caddy
echo 'example.com {
    reverse_proxy 127.0.0.1:8081
}' | sudo tee /etc/caddy/Caddyfile
sudo systemctl restart caddy          # restart, not reload: it may not be running yet
sudo systemctl status caddy --no-pager
```

and set `WEB_PORT=8081` in `.env` so Caddy owns ports 80 and 443. Replace
`example.com` with your domain, and point its DNS at the server first; Caddy
fetches the certificate itself.

If Caddy will not start, `sudo journalctl -u caddy -n 30` says why — most
often another program already holds port 80 or 443
(`sudo ss -tlnp | grep -E ':(80|443)'` shows which).

### 6. Optional repository variable

`VITE_WALLETCONNECT_ID` — a WalletConnect (Reown) project id. Without it the
WalletConnect option is hidden; injected wallets such as MetaMask still work.
It is compiled into the page, so it is a variable, not a secret.

No secrets need adding: the workflow uses the built-in `GITHUB_TOKEN` both to
push the images and, on the server, to pull them.

## Releasing

```
git tag v0.1.0
git push origin v0.1.0
```

Follow it under the repository's **Actions** tab. The deploy job ends by
checking `http://127.0.0.1:$WEB_PORT/healthz` on the server, and prints the
container logs if the site does not come up.

## Operating the stack

From the deploy directory, as the `github` user:

```
docker compose ps                         # what is running
docker compose logs -f api indexer        # follow the backend
TAG=$(cat .deployed-tag) docker compose up -d   # restart the deployed release
```

**Rolling back:** re-run the deploy job of an earlier tag from the Actions tab,
or on the server:

```
TAG=v0.1.0 docker compose up -d
```

**Backups:** the database lives in the `early-pair2_pgdata` volume.

```
docker compose exec -T postgres pg_dump -U earlypair earlypair | gzip > backup-$(date +%F).sql.gz
```

## Things to know

- **RPC endpoints are public and region-sensitive.** Some block requests by
  country — Circle's own Arc endpoint answered Cloudflare error 1009 from the
  development machine, so it ships disabled in `config/earlypair.yaml`. From
  the server, check which endpoints answer and override URLs with
  `EARLYPAIR_RPC_URL_<ID>` in `.env`; changing which endpoints are enabled
  needs a config change and a release.
- **The images are private** by default in GitHub Container Registry, which is
  fine: the runner pulls them with the workflow's token.
- **Only the web port is published.** Postgres, Redis and the API's metrics
  port are not reachable from outside the server.
