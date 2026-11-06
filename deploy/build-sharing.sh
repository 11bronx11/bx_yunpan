#!/usr/bin/env bash
# Incremental build using the host's existing Go/npm dependencies. The regular
# deploy/build.sh remains the full cold-build path.
set -euo pipefail
deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
root_dir=$(cd -- "$deploy_dir/.." && pwd)
output=${SHARING_BUILD_DIR:-$root_dir/artifacts/sharing-20261106/images}
tag=${SHARING_IMAGE_TAG:-sharing-20261106-r2}
go_bin=${SHARING_GO_BIN:-/home/bronx/.local/go/bin/go}
mkdir -p "$output"
output=$(cd -- "$output" && pwd)
cd "$root_dir/backend"
for role in api outbox-relay search-indexer; do
  echo "Compiling $role"
  CGO_ENABLED=0 GOMAXPROCS=2 "$go_bin" build -p=1 -trimpath -ldflags='-s -w' -o "$output/service" "./cmd/$role"
  base="bx-yunpan-$role:${SHARING_BASE_TAG:-fullchain-20261106-r2}"
  printf 'FROM %s\nCOPY --chown=65532:65532 service /app/service\n' "$base" > "$output/Dockerfile"
  docker build --memory 768m --memory-swap 1024m --cpu-period 100000 --cpu-quota 100000 \
    -t "bx-yunpan-$role:$tag" "$output"
done
cd "$root_dir/web"
echo 'Building Web'
CI=true NODE_OPTIONS=--max-old-space-size=768 node node_modules/react-scripts/bin/react-scripts.js build
# Stage only the small generated site, never node_modules or configuration.
cp -a build/. "$output/site/"
cp nginx-app.conf "$output/nginx-app.conf"
cat > "$output/Dockerfile" <<'EOF'
FROM nginx:1.29-alpine
LABEL org.opencontainers.image.title="BX YunPan Web"
COPY nginx-app.conf /etc/nginx/conf.d/default.conf
COPY site/ /usr/share/nginx/html/
HEALTHCHECK --interval=10s --timeout=8s --start-period=5s --retries=6 CMD wget -q -O /dev/null http://127.0.0.1/healthz && wget -q -O /dev/null http://127.0.0.1/readyz && wget -q -O /dev/null http://127.0.0.1/storage-healthz || exit 1
EOF
docker build --memory 512m --memory-swap 768m --cpu-period 100000 --cpu-quota 100000 \
  -t "bx-yunpan-web:$tag" "$output"
