#!/bin/bash
set +e

BIN=/tmp/mini-docker-test-bin

sudo killall mini-docker 2>/dev/null; sleep 1
sudo rm -rf /var/run/mini-docker /var/log/mini-docker /var/lib/mini-docker
sudo mkdir -p /var/run/mini-docker /var/log/mini-docker /var/lib/mini-docker/images

IMGDIR=/var/lib/mini-docker/images/alpine/rootfs
sudo mkdir -p "$IMGDIR"/{bin,usr/bin,proc,sys,dev,tmp,etc,lib,lib64}
for bin in /bin/sh /bin/echo /bin/ls /bin/cat /bin/sleep; do
    if [ -f "$bin" ]; then
        sudo cp "$bin" "$IMGDIR"/bin/ 2>/dev/null || true
    fi
done

sudo "$BIN" daemon > /tmp/mini-docker-test5.log 2>&1 &
sleep 3

echo "--- Running with --network=none ---"
OUT=$(sudo "$BIN" run -d --network=none alpine /bin/sh -c "echo hello_none; sleep 120" 2>&1)
echo "RAW OUTPUT:"
echo "$OUT"

echo ""
echo "Extracted ID:"
echo "$OUT" | grep -oP '"id":\s*"\K[^"]+' | head -1

sudo killall mini-docker 2>/dev/null
