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

# Copy the binary aside and move it into place: overwriting it directly fails
# with ETXTBSY while the app is running, and mv swaps the directory entry
# without disturbing the running process (it picks up the new one next launch).
scp build/pitwall "root@$DEVICE:$APPDIR/.pitwall.new"
ssh "root@$DEVICE" "mv -f $APPDIR/.pitwall.new $APPDIR/pitwall && chmod +x $APPDIR/pitwall"
scp external.manifest.json "root@$DEVICE:$APPDIR/external.manifest.json"
# Launcher icon.
if [ -f icon.png ]; then
    scp icon.png "root@$DEVICE:$APPDIR/icon.png"
fi

echo "Done. Open AppLoad on the device and launch 'Pit Wall'."
