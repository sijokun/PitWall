#!/bin/sh
# Build Pit Wall for the reMarkable Paper Pro (aarch64 Linux) and install as an
# AppLoad external app. Usage: ./deploy.sh [device-ip]   (default 10.11.99.1 = USB)
set -e
DEVICE="${1:-10.11.99.1}"
APPDIR=/home/root/xovi/exthome/appload/pitwall

echo "Building aarch64 binary..."
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o build/pitwall ./cmd/pitwall

echo "Installing to root@$DEVICE:$APPDIR ..."
ssh "root@$DEVICE" "mkdir -p $APPDIR"

# If .env defines F1_TOKEN, inject it into the manifest's "environment" at
# deploy time so the secret never lives in the committed manifest.
MANIFEST=external.manifest.json
if [ -f .env ]; then
    set -a; . ./.env; set +a
fi
if [ -n "${F1_TOKEN:-}" ]; then
    echo "Injecting F1_TOKEN from .env into manifest..."
    MANIFEST=build/external.manifest.json
    F1_TOKEN="$F1_TOKEN" python3 - <<'EOF'
import json, os
m = json.load(open("external.manifest.json"))
m.setdefault("environment", {})["F1_TOKEN"] = os.environ["F1_TOKEN"]
json.dump(m, open("build/external.manifest.json", "w"), indent=4)
EOF
fi
# Copy the binary aside and move it into place: overwriting it directly fails
# with ETXTBSY while the app is running, and mv swaps the directory entry
# without disturbing the running process (it picks up the new one next launch).
scp build/pitwall "root@$DEVICE:$APPDIR/.pitwall.new"
ssh "root@$DEVICE" "mv -f $APPDIR/.pitwall.new $APPDIR/pitwall && chmod +x $APPDIR/pitwall"
scp "$MANIFEST" "root@$DEVICE:$APPDIR/external.manifest.json"
# Launcher icon (generate/choose with: go run ./cmd/icon -style track).
if [ -f icon.png ]; then
    scp icon.png "root@$DEVICE:$APPDIR/icon.png"
fi

echo "Done. Open AppLoad on the device and launch 'Pit Wall'."
