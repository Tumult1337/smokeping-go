#!/usr/bin/env bash
# update-slave.sh — one-command in-place update for a systemd-managed gosmokeping
# slave (hosts that run the binary directly, not via Docker).
#
#   sudo ./update-slave.sh            # pull the latest GitHub release
#   sudo ./update-slave.sh v2.2.0     # pull a specific tag
#
# It auto-detects the service and binary path, downloads the release binary,
# refuses anything that predates the health mesh (so it can't silently
# downgrade a mesh node), backs up the current binary, swaps + restarts, and
# rolls back automatically if the service fails to come up.
set -euo pipefail

REPO="Tumult1337/smokeping-go"
ASSET="gosmokeping-linux-amd64"
TAG="${1:-latest}"

die() { echo "error: $*" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || die "must run as root (need to write the binary and manage systemd)"

# --- locate the slave's unit + binary, without hardcoding either host's layout.
UNIT=""
for u in gosmokeping.service sp.service; do
  if [ -f "/etc/systemd/system/$u" ] && grep -q -- '--slave' "/etc/systemd/system/$u"; then
    UNIT="$u"; break
  fi
done
[ -n "$UNIT" ] || die "no systemd unit running 'gosmokeping --slave' found in /etc/systemd/system"
BIN="$(sed -n 's/^ExecStart=\([^ ]*\).*/\1/p' "/etc/systemd/system/$UNIT" | head -1)"
[ -x "$BIN" ] || die "binary from $UNIT ExecStart not found/executable: $BIN"
echo "unit: $UNIT  binary: $BIN"

# --- download the release asset to a temp file.
if [ "$TAG" = "latest" ]; then
  URL="https://github.com/$REPO/releases/latest/download/$ASSET"
else
  URL="https://github.com/$REPO/releases/download/$TAG/$ASSET"
fi
TMP="$(mktemp)"; trap 'rm -f "$TMP"' EXIT
echo "downloading $URL"
curl -fSL --retry 3 -o "$TMP" "$URL" || die "download failed ($URL)"

# --- sanity-gate the new binary BEFORE touching the running one.
[ -s "$TMP" ] || die "downloaded file is empty"
head -c4 "$TMP" | grep -q $'\x7fELF' || die "downloaded file is not an ELF binary"
chmod +x "$TMP"
# The current latest release may predate the slave health mesh; installing it
# would drop this node out of the mesh. Refuse anything without mesh support.
grep -aq slavehealth "$TMP" || die "release '$TAG' predates the health mesh (no slavehealth support) — cut a newer release from main first"

# --- swap with rollback. Keep a timestamped backup and restore it if the new
# binary won't start (a probe node down is worse than a stale one).
BAK="$BIN.bak-$(date +%Y%m%d-%H%M%S)"
cp -a "$BIN" "$BAK"
echo "backup: $BAK"
systemctl stop "$UNIT"
install -o root -g root -m 0755 "$TMP" "$BIN"
if systemctl start "$UNIT" && sleep 2 && systemctl is-active --quiet "$UNIT"; then
  echo "updated OK — $UNIT is active"
  echo "recent log:"; journalctl -u "$UNIT" -n 3 --no-pager -o cat || true
else
  echo "new binary failed to start — rolling back to $BAK" >&2
  systemctl stop "$UNIT" || true
  install -o root -g root -m 0755 "$BAK" "$BIN"
  systemctl start "$UNIT"
  die "rolled back; $UNIT restarted on the previous binary"
fi
