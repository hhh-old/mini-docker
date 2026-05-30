#!/bin/bash
# Daemon 全链路集成测试
# 测试 start-daemon -> run -> ps -> stop -> rm 完整流程
# 需要在 WSL2/Linux 中以 root 运行
#
# 用法: sudo bash test-daemon.sh [/path/to/mini-docker-binary]

set +e

BIN="${1:-/tmp/mini-docker}"

PASS=0
FAIL=0
SKIP=0

result() {
    if [ "$1" = "PASS" ]; then PASS=$((PASS+1)); elif [ "$1" = "FAIL" ]; then FAIL=$((FAIL+1)); else SKIP=$((SKIP+1)); fi
    echo "  RESULT: $1"
}

CONTAINER_ID=""

echo "=========================================="
echo "  mini-docker Daemon Integration Test"
echo "=========================================="
echo "Date:   $(date)"
echo "Binary: $BIN"
echo ""

if [ ! -f "$BIN" ]; then
    echo "ERROR: binary not found: $BIN"
    exit 1
fi

# ====== TEST 1: start daemon ======
echo "--- TEST 1: start daemon ---"
sudo killall mini-docker 2>/dev/null
sleep 1
sudo rm -rf /var/run/mini-docker /var/log/mini-docker /var/lib/mini-docker
sudo mkdir -p /var/run/mini-docker /var/log/mini-docker /var/lib/mini-docker/images

sudo $BIN daemon > /tmp/mini-docker-daemon.log 2>&1 &
sleep 3
DAEMON_PID=$(pidof mini-docker 2>/dev/null || pgrep -x mini-docker 2>/dev/null || echo "")
if [ -n "$DAEMON_PID" ] && sudo kill -0 $DAEMON_PID 2>/dev/null; then
    echo "  daemon PID: $DAEMON_PID"
    result PASS
else
    echo "  daemon failed to start. Log:"
    tail -10 /tmp/mini-docker-daemon.log 2>/dev/null
    result FAIL
    echo ""
    echo "Aborting - daemon not running"
    exit 1
fi

# ====== TEST 2: prepare local image ======
echo ""
echo "--- TEST 2: prepare local image ---"
IMGDIR=/var/lib/mini-docker/images/alpine/rootfs
sudo mkdir -p $IMGDIR/{bin,usr/bin,proc,sys,dev,tmp,etc,lib,lib64}
for bin in /bin/sh /bin/echo /bin/ls /bin/cat /bin/sleep; do
    if [ -f "$bin" ]; then
        sudo cp "$bin" $IMGDIR/bin/ 2>/dev/null || true
        for lib in $(ldd $bin 2>/dev/null | grep -o '/lib[^ ]*'); do
            dir=$(dirname "$lib")
            sudo mkdir -p "$IMGDIR$dir" 2>/dev/null || true
            sudo cp "$lib" "$IMGDIR$lib" 2>/dev/null || true
        done
    fi
done
echo "  bin contents: $(ls $IMGDIR/bin/ 2>/dev/null | tr '\n' ' ')"
result PASS

# ====== TEST 3: run container ======
echo ""
echo "--- TEST 3: run container ---"
RUN_OUT=$(sudo $BIN run -d alpine /bin/sh -c "echo HELLO_WORLD && sleep 60" 2>&1)
RUN_RC=$?
CONTAINER_ID=$(echo "$RUN_OUT" | grep -oP '"id":\s*"\K[^"]+' | head -1)
if [ -z "$CONTAINER_ID" ]; then
    CONTAINER_ID=$(echo "$RUN_OUT" | grep -oE '[0-9a-f]{32}' | head -1)
fi
echo "  container ID: $CONTAINER_ID"
echo "  exit code: $RUN_RC"
if [ $RUN_RC -eq 0 ] && [ -n "$CONTAINER_ID" ]; then
    result PASS
else
    echo "  full output (first 5 lines):"
    echo "$RUN_OUT" | head -5
    result FAIL
fi

# ====== TEST 4: ps ======
echo ""
echo "--- TEST 4: ps list containers ---"
sleep 3
PS_OUT=$(sudo $BIN ps 2>&1)
echo "  $PS_OUT"
if echo "$PS_OUT" | grep -qE "alpine|running"; then
    result PASS
else
    result FAIL
fi

# ====== TEST 5: stop ======
echo ""
echo "--- TEST 5: stop container ---"
if [ -n "$CONTAINER_ID" ]; then
    STOP_OUT=$(sudo $BIN stop "$CONTAINER_ID" 2>&1)
    STOP_RC=$?
    echo "  output: $STOP_OUT"
    echo "  exit code: $STOP_RC"
    sleep 2
    if [ $STOP_RC -eq 0 ]; then result PASS; else result FAIL; fi
else
    result SKIP
fi

# ====== TEST 6: ps -a after stop ======
echo ""
echo "--- TEST 6: ps -a shows exited container ---"
PSA_OUT=$(sudo $BIN ps -a 2>&1)
echo "  $PSA_OUT"
if echo "$PSA_OUT" | grep -qi "exited"; then
    result PASS
else
    result FAIL
fi

# ====== TEST 7: rm ======
echo ""
echo "--- TEST 7: rm container ---"
if [ -n "$CONTAINER_ID" ]; then
    RM_OUT=$(sudo $BIN rm "$CONTAINER_ID" 2>&1)
    RM_RC=$?
    echo "  output: $RM_OUT"
    echo "  exit code: $RM_RC"
    if [ $RM_RC -eq 0 ]; then result PASS; else result FAIL; fi
else
    result SKIP
fi

# ====== cleanup ======
sudo killall mini-docker 2>/dev/null
sleep 1

echo ""
echo "=========================================="
echo "  Results: PASS=$PASS  FAIL=$FAIL  SKIP=$SKIP"
echo "=========================================="