#!/usr/bin/env bash
# Deploys code-mcp to an Ubuntu machine (linux-arm64 by default), the same way
# deploy-ubuntu.ps1 does from Windows: cross-compile, copy the binary and the
# systemd unit, then install and start the service.
#
# No config file is deployed. Everything the device needs is on the ExecStart
# line, so a redeploy cannot clobber a config.json placed on the machine by
# hand - the server reads one from /opt/code-mcp if it is there.
#
# Usage: ./deploy.sh [--host 192.168.0.162] [--user ubuntu] [--arch arm64]
#                    [--service-user codemcp] [--workspace /srv/code-mcp/workspace]
#                    [--url http://0.0.0.0:8765/mcp] [--token secret]
#                    [--allow-origin https://example.com]
#
# Authentication is whatever ssh already does for you: a key, or the password
# it prompts for. sudo on the device runs through an allocated tty (ssh -t).
set -euo pipefail

cd "$(dirname "$0")"

host=192.168.0.162
user=ubuntu
arch=arm64
service_user=codemcp
workspace=/srv/code-mcp/workspace
listen_url=http://0.0.0.0:8765/mcp
auth_token=
allow_origin=

while [ $# -gt 0 ]; do
  case "$1" in
  --host) host="$2"; shift 2 ;;
  --user) user="$2"; shift 2 ;;
  --arch) arch="$2"; shift 2 ;;
  --service-user) service_user="$2"; shift 2 ;;
  --workspace) workspace="$2"; shift 2 ;;
  --url) listen_url="$2"; shift 2 ;;
  --token) auth_token="$2"; shift 2 ;;
  --allow-origin) allow_origin="$2"; shift 2 ;;
  -h | --help) sed -n '2,18p' "$0"; exit 0 ;;
  *) echo "unknown argument: $1 (try --help)" >&2; exit 1 ;;
  esac
done

binary="dist/codemcp-linux-${arch}"
unit=deploy/code-mcp.service
remote_dir=/tmp/code-mcp-deploy
target="${user}@${host}"

[ -f "$unit" ] || { echo "missing $unit" >&2; exit 1; }
[ -n "$auth_token" ] || echo "warning: no --token: anything that can reach ${listen_url} can drive this server" >&2

if [ ! -f "$binary" ]; then
  echo "==> $binary missing; cross-compiling for linux/${arch} ..."
  version="$(git describe --tags --always 2>/dev/null || echo 0.1.0000-dev)"
  mkdir -p dist
  GOOS=linux GOARCH="$arch" CGO_ENABLED=0 \
    go build -trimpath -ldflags "-s -w -X main.version=${version}" -o "$binary" .
fi

# Stage everything in one directory so a single recursive copy moves it all.
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
install -m 755 "$binary" "$stage/codemcp"
# The unit ships with placeholders so the service user, workspace, listen URL,
# auth token and allowed origin are decided here rather than being baked into
# the repository.
token_flag=""
[ -n "$auth_token" ] && token_flag=" --token ${auth_token}"
origin_flag=""
[ -n "$allow_origin" ] && origin_flag=" --allow-origin ${allow_origin}"
sed -e "s|__USER__|${service_user}|g" \
  -e "s|__WORKSPACE__|${workspace}|g" \
  -e "s|__URL__|${listen_url}|g" \
  -e "s|__TOKEN_FLAG__|${token_flag}|g" \
  -e "s|__ORIGIN_FLAG__|${origin_flag}|g" \
  "$unit" > "$stage/code-mcp.service"

cat > "$stage/install.sh" <<INSTALL
set -e
cd ${remote_dir}
id -u ${service_user} >/dev/null 2>&1 || useradd --system --home /opt/code-mcp --shell /usr/sbin/nologin ${service_user}
mkdir -p /opt/code-mcp ${workspace}
# stop first so the running binary can be replaced
echo "Stopping code-mcp service..." && systemctl stop code-mcp.service 2>/dev/null || true
echo "Installing codemcp..." && install -m 755 codemcp /opt/code-mcp/codemcp
# a symlink so the same binary is on PATH for interactive use
ln -sf /opt/code-mcp/codemcp /usr/local/bin/codemcp
# no config.json is deployed; the ExecStart flags carry the settings, and a
# config.json placed on the device by hand is left exactly as it is
chown -R ${service_user}:${service_user} /opt/code-mcp ${workspace}
install -m 644 code-mcp.service /etc/systemd/system/code-mcp.service
systemctl daemon-reload
echo "Enabling code-mcp service..." && systemctl enable --now code-mcp.service
systemctl restart code-mcp.service
sleep 2
systemctl is-enabled code-mcp.service
systemctl is-active code-mcp.service
echo "Getting code-mcp status..." && systemctl status code-mcp.service --no-pager -n 15 || true
rm -rf ${remote_dir}
INSTALL

echo "==> Copying files to ${target}:${remote_dir} ..."
ssh "$target" "rm -rf ${remote_dir} && mkdir -p ${remote_dir}"
scp -r "$stage"/* "${target}:${remote_dir}/"

echo "==> Installing and starting code-mcp.service ..."
ssh -t "$target" "sudo bash ${remote_dir}/install.sh"

echo "==> Deployed. The server is at http://${host}:8765/mcp"
echo "    Follow logs with:  ssh ${target} journalctl -u code-mcp -f"
[ -n "$auth_token" ] || echo "    Redeploy with --token <secret> to require a bearer token." 
