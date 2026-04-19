#!/usr/bin/env bash
#
# Install gosmokeping as a systemd service. Idempotent — safe to re-run to
# update the binary or the unit file.
#
# Usage (from the repo root, after `make build`):
#   sudo ./deploy/install.sh
#
set -euo pipefail

BIN_SRC="${BIN_SRC:-./gosmokeping}"
BIN_DST="${BIN_DST:-/usr/local/bin/gosmokeping}"
CONFIG_DIR="${CONFIG_DIR:-/etc/gosmokeping}"
STATE_DIR="${STATE_DIR:-/var/lib/gosmokeping}"
UNIT_SRC="${UNIT_SRC:-./deploy/gosmokeping.service}"
UNIT_DST="${UNIT_DST:-/etc/systemd/system/gosmokeping.service}"
SVC_USER="${SVC_USER:-gosmokeping}"
SVC_GROUP="${SVC_GROUP:-gosmokeping}"

if [[ $EUID -ne 0 ]]; then
  echo "install.sh must run as root (try: sudo $0)" >&2
  exit 1
fi

if [[ ! -x "$BIN_SRC" ]]; then
  echo "binary not found at $BIN_SRC — run 'make build' first" >&2
  exit 1
fi

if [[ ! -f "$UNIT_SRC" ]]; then
  echo "unit file not found at $UNIT_SRC" >&2
  exit 1
fi

echo "==> creating service group '$SVC_GROUP'"
if ! getent group "$SVC_GROUP" >/dev/null; then
  groupadd --system "$SVC_GROUP"
fi

echo "==> creating service user '$SVC_USER'"
if ! id -u "$SVC_USER" >/dev/null 2>&1; then
  useradd --system \
    --gid "$SVC_GROUP" \
    --home-dir "$STATE_DIR" \
    --no-create-home \
    --shell /usr/sbin/nologin \
    --comment "gosmokeping service" \
    "$SVC_USER"
fi

echo "==> creating directories"
install -d -m 0755                                "$CONFIG_DIR"
install -d -m 0750 -o "$SVC_USER" -g "$SVC_GROUP" "$STATE_DIR"

echo "==> installing binary to $BIN_DST"
install -m 0755 "$BIN_SRC" "$BIN_DST"

echo "==> installing systemd unit to $UNIT_DST"
install -m 0644 "$UNIT_SRC" "$UNIT_DST"

echo "==> reloading systemd"
systemctl daemon-reload

cat <<EOF

Installed.

Next steps:
  1. Place your config:
       sudo cp config.example.json $CONFIG_DIR/config.json
       sudo cp .env.example        $CONFIG_DIR/.env
       sudo \$EDITOR $CONFIG_DIR/config.json $CONFIG_DIR/.env
       sudo chown root:$SVC_GROUP $CONFIG_DIR/.env
       sudo chmod 0640            $CONFIG_DIR/.env

  2. Enable and start:
       sudo systemctl enable --now gosmokeping

  3. Tail logs:
       sudo journalctl -u gosmokeping -f

To reload config without restart:  sudo systemctl reload gosmokeping
To uninstall:  sudo systemctl disable --now gosmokeping && \\
               sudo rm $BIN_DST $UNIT_DST && \\
               sudo systemctl daemon-reload
EOF
