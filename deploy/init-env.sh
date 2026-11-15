#!/usr/bin/env bash
set -euo pipefail

deploy_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
env_file="$deploy_dir/.env"
template_file="$deploy_dir/.env.example"

if [[ ${1:-} == "--no-ai" ]]; then
  template_file="$deploy_dir/.env.no-ai.example"
elif (($#)); then
  echo "Usage: ./deploy/init-env.sh [--no-ai]" >&2
  exit 2
fi

if [[ -f "$env_file" ]]; then
  python3 - "$env_file" "$template_file" <<'PYENV'
import re,sys
path,template=sys.argv[1:]
current=open(path).read(); keys=set(re.findall(r'^([A-Z][A-Z0-9_]*)=',current,re.M))
missing=[line for line in open(template) if re.match(r'^[A-Z][A-Z0-9_]*=',line) and line.split('=',1)[0] not in keys]
if missing:
    with open(path,'a') as f:f.write('\n# Added compatible configuration defaults\n'+''.join(missing))
print('Environment preserved; added %d missing keys.'%len(missing))
PYENV
  exit 0
fi
if [[ ! -f "$template_file" ]]; then
  echo "Missing environment template: $template_file" >&2
  exit 1
fi

random_secret() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
    return
  fi
  od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
}

umask 077
if command -v docker >/dev/null 2>&1; then
  for volume in bx-yunpan_postgres-data bx-yunpan_minio-data; do
    if docker volume inspect "$volume" >/dev/null 2>&1; then
      echo "Cannot create a new .env while bx-yunpan data volumes already exist." >&2
      echo "Restore the original deploy/.env or remove the data volumes explicitly." >&2
      exit 1
    fi
  done
fi
postgres_password=$(random_secret)
minio_password=$(random_secret)
auth_seed=$(random_secret)
share_secret=$(random_secret)
grafana_password=$(random_secret)

cp "$template_file" "$env_file"
set_env() {
  local key=$1 value=$2
  sed -i "s|^${key}=.*$|${key}=${value}|" "$env_file"
}

set_env POSTGRES_PASSWORD "$postgres_password"
set_env MINIO_ROOT_PASSWORD "$minio_password"
set_env AUTH_SIGNING_SEED "$auth_seed"
set_env SHARE_SECRET "$share_secret"
set_env GRAFANA_ADMIN_PASSWORD "$grafana_password"
set_env RABBITMQ_PASSWORD "$(random_secret)"
set_env ASYNC_POSTGRES_PASSWORD "$(random_secret)"
set_env POSTGRES_READER_PASSWORD "$(random_secret)"
chmod 600 "$env_file"

echo "Created $env_file from $(basename "$template_file") with local random secrets."
