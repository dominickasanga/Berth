#!/usr/bin/env bash
# Update Berth in place: pull the latest code, build it, swap the binary, restart the service.
# Run it from the Berth folder on the server:   ./update.sh
# Needs: git, Go, internet access for Go modules. The database is migrated by Berth itself on start.
set -euo pipefail
cd "$(dirname "$0")"

before=$(git rev-parse --short HEAD)
git pull --ff-only
after=$(git rev-parse --short HEAD)
echo "source: $before -> $after"

go build -o berth.new ./cmd/berth      # build beside the running binary; never overwrite it in place
[ -x berth ] && cp -p berth berth.prev # keep the previous build for a quick way back
mv -f berth.new berth                  # an atomic swap: safe while the old binary is still running
echo "built:  $(./berth version)"

if systemctl is-active --quiet berth 2>/dev/null; then
  sudo systemctl restart berth
  sleep 2
  systemctl is-active berth
  journalctl -u berth -n 6 --no-pager
else
  echo "Berth is not running as a systemd service: restart it by hand (Ctrl+C, then ./berth serve)."
fi
echo "way back: mv -f berth.prev berth && sudo systemctl restart berth"
