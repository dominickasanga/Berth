#!/bin/sh
# TusK sender installer. Served by the receiver, run on the machine to be backed up:
#
#   curl -fsSL -u <user>:<password> https://RECEIVER/beam/install.sh | \
#       TUSK_PASSWORD=<password> sh -s -- /var/www/finance_api/media
#
# It installs the agent, writes a STARTER config, and installs the systemd timer.
# The config is yours after that: this script never overwrites an existing one,
# so re-running it to upgrade the binary cannot silently change what you back up.
set -eu

SERVER_URL="__SERVER_URL__"
MEDIA_ROOT="${1:-/var/www/finance_api/media}"

BIN=/usr/local/bin/tusk-agent
CONF_DIR=/etc/tusk
STATE_DIR=/var/lib/tusk
UNIT_DIR=/etc/systemd/system

say() { printf '%s\n' "$*"; }
die() { printf '❌ %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || die "Run as root (installs to $BIN and $UNIT_DIR)."
[ -n "${TUSK_PASSWORD:-}" ] || die "Set TUSK_PASSWORD to the receiver's upload password."
[ -d "$MEDIA_ROOT" ] || die "Media root '$MEDIA_ROOT' does not exist. Pass the correct path as the first argument."

# The receiver serves one architecture. Refuse now with a clear message rather
# than install a binary that fails with "exec format error" at 23:30.
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64) ;;
    *) die "This receiver serves x86_64 binaries; this host is $ARCH. Build the agent from source, or rebuild the receiver image with --build-arg TARGET_ARCH=$ARCH." ;;
esac

say "🐘 Installing TusK sender"
say "   receiver:   $SERVER_URL"
say "   media root: $MEDIA_ROOT"

# --- binary ---------------------------------------------------------------
# Downloaded to a temp file and moved into place, so a failed download cannot
# leave a truncated binary that systemd will happily try to run.
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT
say "⬇️  Downloading agent"
curl -fsSL -u "admin:${TUSK_PASSWORD}" "${SERVER_URL}get-agent" -o "$TMP" \
    || die "Download failed. Check the URL and TUSK_PASSWORD."
chmod 0755 "$TMP"
mv "$TMP" "$BIN"
trap - EXIT
say "   installed $BIN"

# --- directories ----------------------------------------------------------
mkdir -p "$CONF_DIR" "$STATE_DIR/staging"
chmod 0750 "$CONF_DIR" "$STATE_DIR"

# --- credentials ----------------------------------------------------------
if [ -f "$CONF_DIR/agent.env" ]; then
    say "   keeping existing $CONF_DIR/agent.env"
else
    umask 077
    printf 'TUSK_PASSWORD=%s\n' "$TUSK_PASSWORD" > "$CONF_DIR/agent.env"
    chmod 0600 "$CONF_DIR/agent.env"
    say "   wrote $CONF_DIR/agent.env (0600)"
fi

# --- config ---------------------------------------------------------------
# Never overwritten: the source list is owned by this host, not by the receiver.
if [ -f "$CONF_DIR/agent.json" ]; then
    say "   keeping existing $CONF_DIR/agent.json (edit it yourself to change sources)"
else
    cat > "$CONF_DIR/agent.json" <<JSON
{
  "server": "${SERVER_URL}",
  "username": "admin",
  "state_dir": "${STATE_DIR}",
  "staging_dir": "${STATE_DIR}/staging",
  "max_duration": "6h",

  "sources": [
    {
      "name": "backups",
      "path": "${MEDIA_ROOT}/backups",
      "mode": "latest",
      "pattern": "*.dump.tar.gz"
    },
    {
      "name": "documents",
      "path": "${MEDIA_ROOT}/documents",
      "mode": "incremental"
    },
    {
      "name": "profile_images",
      "path": "${MEDIA_ROOT}/profile_images",
      "mode": "incremental"
    }
  ]
}
JSON
    chmod 0640 "$CONF_DIR/agent.json"
    say "   wrote $CONF_DIR/agent.json — review it before the first run"
fi

# --- systemd --------------------------------------------------------------
cat > "$UNIT_DIR/tusk-agent.service" <<UNIT
[Unit]
Description=TusK backup agent (ships changed files offsite)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=root
Group=root
WorkingDirectory=${STATE_DIR}
EnvironmentFile=${CONF_DIR}/agent.env
ExecStart=${BIN} -config ${CONF_DIR}/agent.json

# SIGTERM lets the transfer finish its current chunk and save the resume offset,
# so a run stopped at the end of its window continues instead of starting over.
KillSignal=SIGTERM
TimeoutStopSec=120

StandardOutput=journal
StandardError=journal

ProtectSystem=strict
ReadWritePaths=${STATE_DIR}
ReadOnlyPaths=${MEDIA_ROOT}
PrivateTmp=true
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
UNIT

cat > "$UNIT_DIR/tusk-agent.timer" <<UNIT
[Unit]
Description=Nightly TusK backup run

[Timer]
# 23:30 local, half an hour after the 23:00 database dump, so the dump and its
# tarball are finished and renamed into place before we look.
OnCalendar=*-*-* 23:30:00
Persistent=true
RandomizedDelaySec=300

[Install]
WantedBy=timers.target
UNIT

systemctl daemon-reload
systemctl enable --now tusk-agent.timer >/dev/null 2>&1 || systemctl enable tusk-agent.timer
say "   installed and enabled tusk-agent.timer"

# --- verify ---------------------------------------------------------------
say ""
say "🔍 Verifying"
set +e
# shellcheck disable=SC1090
. "$CONF_DIR/agent.env"
TUSK_PASSWORD="$TUSK_PASSWORD" "$BIN" -config "$CONF_DIR/agent.json" -check
RC=$?
set -e

say ""
if [ "$RC" -ne 0 ]; then
    say "⚠️  Installed, but the checks above failed. Fix them, then re-run:"
    say "      $BIN -config $CONF_DIR/agent.json -check"
    exit "$RC"
fi

say "✅ Sender installed."
say "   Next run:   systemctl list-timers tusk-agent.timer"
say "   Run now:    systemctl start tusk-agent.service && journalctl -u tusk-agent -f"
say "   Dry run:    $BIN -config $CONF_DIR/agent.json -dry-run"
