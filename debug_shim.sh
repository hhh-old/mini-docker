#!/bin/bash
LOCAL="/tmp/mini-docker-v5"
REMOTE="/mnt/d/project/Claudecode/testdocker/mini-docker/mini-docker"

echo 123456 | sudo -S killall -9 mini-docker-v4 2>/dev/null
echo 123456 | sudo -S killall -9 mini-docker-v5 2>/dev/null
sleep 2

echo "=== Copy binary ==="
echo 123456 | sudo -S cp $REMOTE $LOCAL 2>&1
echo 123456 | sudo -S chmod +x $LOCAL

echo "=== Clean up ==="
echo 123456 | sudo -S rm -rf /var/run/mini-docker /var/log/mini-docker /var/lib/mini-docker
echo 123456 | sudo -S mkdir -p /var/run/mini-docker /var/log/mini-docker /var/lib/mini-docker/images

echo "=== Start daemon ==="
echo 123456 | sudo -S $LOCAL daemon &
sleep 2

echo "=== Pull image ==="
echo 123456 | sudo -S $LOCAL pull ubuntu 2>&1

echo "=== Run container ==="
echo 123456 | sudo -S timeout 30 $LOCAL run -d ubuntu sleep 9999 2>&1
echo "Run exit code: $?"

echo "=== Wait 3s ==="
sleep 3

echo "=== ps ==="
echo 123456 | sudo -S $LOCAL ps 2>&1

echo "=== Check processes ==="
echo 123456 | sudo -S ps aux | grep 'sleep 9999' | grep -v grep || echo "No sleep processes"

echo "=== Cleanup ==="
echo 123456 | sudo -S killall -9 mini-docker-v5 2>/dev/null
