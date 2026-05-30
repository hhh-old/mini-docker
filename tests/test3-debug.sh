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

sudo "$BIN" daemon > /tmp/mini-docker-test3.log 2>&1 &
sleep 3

OUT=$(sudo "$BIN" run -d -p 8080:80 alpine /bin/sh -c "sleep 120" 2>&1)
echo "Container output:"
echo "$OUT" | head -5

echo ""
echo "--- iptables nat table ---"
sudo iptables-legacy -t nat -L 2>&1 | head -30

echo ""
echo "--- MD chain ---"
sudo iptables-legacy -t nat -L MD 2>&1

echo ""
echo "--- grep for 8080 ---"
sudo iptables-legacy -t nat -L 2>&1 | grep 8080

sudo killall mini-docker 2>/dev/null
