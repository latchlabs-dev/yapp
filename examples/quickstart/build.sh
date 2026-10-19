#!/bin/sh
# Never send deployment secrets, local runtime data, or Git history to Docker.
set -eu
cd "$(dirname "$0")/../.."
context=$(mktemp -d "${TMPDIR:-/tmp}/piproxy-build.XXXXXX")
trap 'rm -rf "$context"' EXIT HUP INT TERM
for path in Dockerfile go.mod go.sum cmd internal third_party migrations extensions; do
  rsync -a --exclude=node_modules --exclude=.git --exclude=".env*" \
    --exclude=secrets --exclude=data --exclude=__pycache__ --exclude=.venv \
    "$path" "$context/"
done
# The Go secrets package is source code, unlike runtime secrets directories.
mkdir -p "$context/internal/secrets"
cp internal/secrets/*.go "$context/internal/secrets/"
cat > "$context/.dockerignore" <<'IGNORE'
**/node_modules
**/.git
**/.env*
**/secrets
!internal/secrets
!internal/secrets/*.go
**/data
**/__pycache__
**/.venv
**/*.sa.json
IGNORE
docker build -t piproxy-quickstart:local "$context"
# UI context is similarly allowlisted and contains no .env or node_modules.
mkdir "$context/ui"
for path in Dockerfile nginx.conf package.json pnpm-lock.yaml .npmrc index.html src public tsconfig.json tsconfig.app.json tsconfig.node.json vite.config.ts; do
  if [ -e "ui/$path" ]; then rsync -a --exclude=node_modules --exclude=.git --exclude=".env*" --exclude=secrets --exclude=data --exclude="*.sa.json" "ui/$path" "$context/ui/"; fi
done
cp "$context/.dockerignore" "$context/ui/.dockerignore"
docker build -t piproxy-quickstart-ui:local "$context/ui"
