#!/usr/bin/env bash
# Incremental build with the cached, pinned Go/Node dependencies on this host.
set -euo pipefail
deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root_dir=$(cd -- "$deploy_dir/.." && pwd)
output=${SHARING_LIFECYCLE_BUILD_DIR:-$root_dir/artifacts/sharing-lifecycle-20261113/images}
tag=${SHARING_LIFECYCLE_IMAGE_TAG:-sharing-lifecycle-20261113}
mkdir -p "$output"
output=$(cd -- "$output" && pwd)
module_cache=$(cd "$root_dir/backend" && go env GOMODCACHE)
build_cache=$(cd "$root_dir/backend" && go env GOCACHE)

for role in migrate api; do
  role_output="$output/$role"
  mkdir -p "$role_output"
  echo "Compiling $role with Go 1.25.13"
  docker run --rm --memory 1536m --memory-swap 2048m --cpus 2 \
    --user "$(id -u):$(id -g)" -v "$root_dir/backend:/src:ro" -w /src \
    -v "$module_cache:/gomodcache" -v "$build_cache:/gocache" -v "$role_output:/out" \
    -e GOMODCACHE=/gomodcache -e GOCACHE=/gocache -e GOMAXPROCS=2 \
    -e CGO_ENABLED=0 -e GOPROXY=off -e GOTOOLCHAIN=local \
    golang:1.25.13-bookworm go build -mod=readonly -p=1 -trimpath \
    -ldflags='-s -w' -o /out/service "./cmd/$role"
  if [[ "$role" == api ]]; then
    base=bx-yunpan-api:sharing-20261106-r2
  else
    base=bx-yunpan-migrate:fullchain-20261106-r2
  fi
  cp -a "$root_dir/backend/migrations" "$role_output/"
  printf 'FROM %s\nCOPY --chown=65532:65532 service /app/service\nCOPY --chown=65532:65532 migrations /app/migrations\n' "$base" > "$role_output/Dockerfile"
  docker build --memory 768m --memory-swap 1024m --cpu-period 100000 --cpu-quota 100000 \
    -t "bx-yunpan-$role:$tag" "$role_output"
done

cd "$root_dir/web"
CI=true NODE_OPTIONS=--max-old-space-size=768 node node_modules/react-scripts/bin/react-scripts.js build
web_output="$output/web"
mkdir -p "$web_output/site"
cp -a build/. "$web_output/site/"
cp nginx-app.conf "$web_output/nginx-app.conf"
cat > "$web_output/Dockerfile" <<'EOF'
FROM nginx:1.29-alpine
LABEL org.opencontainers.image.title="BX YunPan Web"
COPY nginx-app.conf /etc/nginx/conf.d/default.conf
COPY site/ /usr/share/nginx/html/
HEALTHCHECK --interval=10s --timeout=8s --start-period=5s --retries=6 CMD wget -q -O /dev/null http://127.0.0.1/healthz && wget -q -O /dev/null http://127.0.0.1/readyz && wget -q -O /dev/null http://127.0.0.1/storage-healthz || exit 1
EOF
docker build --memory 512m --memory-swap 768m --cpu-period 100000 --cpu-quota 100000 \
  -t "bx-yunpan-web:$tag" "$web_output"
