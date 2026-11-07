#!/bin/sh
set -eu

if [ "$(uname -s)" != Linux ]; then
  printf '%s\n' 'The daemon installer supports Linux only.' >&2
  exit 1
fi
if [ "$(id -u)" != 0 ]; then
  printf '%s\n' 'Run this installer as root.' >&2
  exit 1
fi
binary=${1:-bin/repodeploy}
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
for executable in git ssh docker systemctl useradd usermod getent install sed mktemp; do
  command -v "$executable" >/dev/null || { printf 'Missing prerequisite: %s\n' "$executable" >&2; exit 1; }
done
test -f "$binary"
getent group docker >/dev/null || { printf '%s\n' 'Install Docker Engine and its docker group first.' >&2; exit 1; }
docker info >/dev/null
docker compose version >/dev/null
if systemctl is-active --quiet repodeploy || systemctl is-active --quiet pushvori; then
  printf '%s\n' 'Stop repodeploy/pushvori and back up its state before replacing the binary.' >&2
  exit 1
fi
service_account=repodeploy
state_dir=/var/lib/repodeploy
if [ ! -e "$state_dir" ] && [ -d /var/lib/pushvori ]; then
  service_account=pushvori
  state_dir=/var/lib/pushvori
fi
if ! id "$service_account" >/dev/null 2>&1; then
  useradd --system --user-group --home-dir "$state_dir" --shell /usr/sbin/nologin "$service_account"
fi
usermod -aG docker "$service_account"
install -d -o "$service_account" -g "$service_account" -m 0700 "$state_dir"
install -m 0755 "$binary" /usr/local/bin/repodeploy
unit_file=$(mktemp)
trap 'rm -f "$unit_file"' EXIT HUP INT TERM
sed -e "s|/var/lib/repodeploy|$state_dir|g" \
  -e "s/^User=repodeploy$/User=$service_account/" \
  -e "s/^Group=repodeploy$/Group=$service_account/" \
  "$script_dir/../packaging/repodeploy.service" > "$unit_file"
install -m 0644 "$unit_file" /etc/systemd/system/repodeploy.service
if [ -f /etc/systemd/system/pushvori.service ]; then
  systemctl disable pushvori
fi
systemctl daemon-reload
printf 'Installed RepoDeploy (account: %s, state: %s). See docs/installation.md before starting repodeploy.\n' "$service_account" "$state_dir"
