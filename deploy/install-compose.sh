#!/bin/sh
# Run with: gh api -H 'Accept: application/vnd.github.raw+json' \
#   repos/JOJO-5/codegate/contents/deploy/install-compose.sh | sh
set -eu

repo=JOJO-5/codegate
install_dir=${CODEGATE_INSTALL_DIR:-"$HOME/.local/share/codegate"}

command -v gh >/dev/null 2>&1 || { echo '需要 GitHub CLI (gh)。' >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo '需要 Docker Desktop / Docker Engine 和 Compose。' >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo 'Docker 尚未启动或当前账号无权访问。' >&2; exit 1; }
docker compose version >/dev/null 2>&1 || { echo '需要 Docker Compose 插件。' >&2; exit 1; }
gh auth status >/dev/null 2>&1 || { echo '请先执行 gh auth login，以访问私有仓库。' >&2; exit 1; }

case $(uname -s) in
  Linux|Darwin) ;;
  *) echo '仅支持 Linux 和 macOS。' >&2; exit 1 ;;
esac

if [ -n "${CODEGATE_DOMAIN:-}" ]; then
  case "$CODEGATE_DOMAIN" in
    *[!a-zA-Z0-9.-]*|.*|*..*|*.) echo 'CODEGATE_DOMAIN 必须是有效域名。' >&2; exit 1 ;;
  esac
fi

umask 077
mkdir -p "$install_dir/deploy" "$install_dir/secrets"
for path in compose.yaml deploy/Caddyfile; do
  tmp=$(mktemp "$install_dir/.codegate.XXXXXXXX")
  if ! gh api -H 'Accept: application/vnd.github.raw+json' "repos/$repo/contents/$path" > "$tmp"; then
    rm -f "$tmp"
    echo "下载 $path 失败，请检查仓库访问权限。" >&2
    exit 1
  fi
  mv "$tmp" "$install_dir/$path"
done

if [ ! -f "$install_dir/secrets/jwt_secret" ]; then
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32 > "$install_dir/secrets/jwt_secret"
  else
    od -An -N32 -tx1 /dev/urandom | tr -d ' \n' > "$install_dir/secrets/jwt_secret"
  fi
fi
chmod 600 "$install_dir/secrets/jwt_secret"

if [ ! -f "$install_dir/.env" ]; then
  if [ -n "${CODEGATE_DOMAIN:-}" ]; then
    printf 'CODEGATE_DOMAIN=%s\nCODEGATE_BASE_URL=https://%s\n' "$CODEGATE_DOMAIN" "$CODEGATE_DOMAIN" > "$install_dir/.env"
  else
    printf 'CODEGATE_BASE_URL=http://127.0.0.1:8080\n' > "$install_dir/.env"
  fi
fi

public_domain=$(sed -n 's/^CODEGATE_DOMAIN=//p' "$install_dir/.env" | tail -n 1)
cd "$install_dir"
# The repository and GHCR image are private; gh's credential is passed through stdin.
gh auth token | docker login ghcr.io -u "$(gh api user --jq .login)" --password-stdin >/dev/null
if [ -n "$public_domain" ]; then
  docker compose --profile public pull server caddy
  docker compose --profile public up -d --no-build
else
  docker compose pull server
  docker compose up -d --no-build server
fi
printf '\nCodeGate 已启动。安装目录：%s\n' "$install_dir"
if [ -n "$public_domain" ]; then
  printf '访问地址：https://%s\n' "$public_domain"
else
  printf '访问地址：http://127.0.0.1:8080 （仅部署机本地）\n'
fi
