#!/usr/bin/env bash
# Runs on the server. The deploy job in .github/workflows/ci.yml copies this
# file and docker-compose.yml into the deploy directory over SSH, then runs it:
#
#   deploy.sh <tag> <image owner>
#
# The registry login happens before it and the logout after it, in the job.
set -euo pipefail

tag=$1
owner=$2
cd "$(dirname "$0")"

if [ ! -f .env ]; then
  echo "::error::$(pwd)/.env is missing. Copy deploy/.env.example there and fill it in (see DEPLOY.md)."
  exit 1
fi

export TAG="$tag" IMAGE_OWNER="$owner"
docker compose pull
docker compose up -d --remove-orphans
# Remember what is running, so a manual `docker compose up -d` in this
# directory starts the same release rather than whatever `latest` is.
echo "$TAG" > .deployed-tag

port="$(grep -E '^WEB_PORT=' .env | tail -1 | cut -d= -f2)"
port="${port:-80}"
for _ in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:${port}/healthz" >/dev/null; then
    echo "Release $TAG is up on port ${port}."
    docker image prune -f --filter "until=168h" >/dev/null || true
    exit 0
  fi
  sleep 5
done
echo "::error::The site did not become healthy within 150s."
docker compose ps
docker compose logs --tail=80 api indexer web
exit 1
