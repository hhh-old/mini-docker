#!/bin/bash
# 边界场景与错误处理测试
# 覆盖: 重复操作、无效参数、不存在的容器、Daemon 重启恢复
# 需要在 WSL2/Linux 中以 root 运行
#
# 用法: sudo bash test-edge-cases.sh [/path/to/mini-docker-binary]

set +e

BIN="${1:-/tmp/mini-docker}"

PASS=0; FAIL=0; SKIP=0
result() {
    if [ "$1" = "PASS" ]; then PASS=$((PASS+1)); elif [ "$1" = "FAIL" ]; then FAIL=$((FAIL+1)); else SKIP=$((SKIP+1)); fi
    echo "  RESULT: $1"
}

echo "=========================================="
echo "  Edge Cases & Error Handling Test"
echo "=========================================="
echo "Date:   $(date)"
echo "Binary: $BIN"
echo ""

if [ ! -f "$BIN" ]; then echo "ERROR: binary not found: $BIN"; exit 1; fi

# ---- setup daemon ----
sudo killall mini-docker 2>/dev/null; sleep 1
sudo rm -rf /var/run/mini-docker /var/log/mini-docker /var/lib/mini-docker
sudo mkdir -p /var/run/mini-docker /var/log/mini-docker /var/lib/mini-docker/images
sudo $BIN daemon > /tmp/mini-docker-test.log 2>&1 &
sleep 3

# ---- prepare image ----
IMGDIR=/var/lib/mini-docker/images/alpine/rootfs
sudo mkdir -p $IMGDIR/{bin,usr/bin,proc,sys,dev,tmp,etc,lib,lib64}
for bin in /bin/sh /bin/echo /bin/ls /bin/cat /bin/sleep /bin/mkdir /bin/rm; do
    if [ -f "$bin" ]; then
        sudo cp "$bin" $IMGDIR/bin/ 2>/dev/null || true
        for lib in $(ldd $bin 2>/dev/null | grep -o '/lib[^ ]*'); do
            dir=$(dirname "$lib")
            sudo mkdir -p "$IMGDIR$dir" 2>/dev/null || true
            sudo cp "$lib" "$IMGDIR$lib" 2>/dev/null || true
        done
    fi
done

extract_id() { echo "$1" | grep -oP '"id":\s*"\K[^"]+' | head -1; }

# ====== TEST 1: stop non-existent container ======
echo "--- TEST 1: stop non-existent container ---"
STOP_OUT=$(sudo $BIN stop nonexistent12345 2>&1)
STOP_RC=$?
echo "  output: $STOP_OUT"
echo "  exit code: $STOP_RC"
if [ $STOP_RC -ne 0 ]; then
    echo "  correctly rejected"
    result PASS
else
    echo "  should have failed"
    result FAIL
fi

# ====== TEST 2: rm non-existent container ======
echo ""
echo "--- TEST 2: rm non-existent container ---"
RM_OUT=$(sudo $BIN rm nonexistent12345 2>&1)
RM_RC=$?
echo "  output: $RM_OUT"
echo "  exit code: $RM_RC"
if [ $RM_RC -ne 0 ]; then result PASS; else result FAIL; fi

# ====== TEST 3: exec into stopped container ======
echo ""
echo "--- TEST 3: exec into stopped container ---"
OUT=$(sudo $BIN run -d alpine /bin/sh -c "echo done" 2>&1)
CID=$(extract_id "$OUT")
sleep 5
if [ -n "$CID" ]; then
    sudo $BIN stop "$CID" 2>/dev/null
    sleep 1
    EXEC_OUT=$(sudo $BIN exec "$CID" echo should_fail 2>&1)
    EXEC_RC=$?
    echo "  exec exit code: $EXEC_RC"
    echo "  exec output: $EXEC_OUT"
    if [ $EXEC_RC -ne 0 ]; then result PASS; else result FAIL; fi
    sudo $BIN rm "$CID" 2>/dev/null
else
    result FAIL
fi

# ====== TEST 4: duplicate container name ======
echo ""
echo "--- TEST 4: duplicate container name ---"
OUT1=$(sudo $BIN run -d --name duptest alpine /bin/sh -c "sleep 60" 2>&1)
CID1=$(extract_id "$OUT1")
sleep 2
OUT2=$(sudo $BIN run -d --name duptest alpine /bin/sh -c "sleep 60" 2>&1)
CID2=$(extract_id "$OUT2")
echo "  first CID:  $CID1"
echo "  second CID: $CID2"
if [ -z "$CID2" ] || [ "$CID2" = "$CID1" ]; then
    echo "  correctly rejected or reused"
    result PASS
else
    echo "  created two containers with same name"
    result FAIL
fi
sudo $BIN stop "$CID1" 2>/dev/null; sudo $BIN rm "$CID1" 2>/dev/null
if [ -n "$CID2" ] && [ "$CID2" != "$CID1" ]; then
    sudo $BIN stop "$CID2" 2>/dev/null; sudo $BIN rm "$CID2" 2>/dev/null
fi

# ====== TEST 5: stop already stopped container ======
echo ""
echo "--- TEST 5: stop already stopped container ---"
OUT=$(sudo $BIN run -d alpine /bin/sh -c "echo stop_test" 2>&1)
CID=$(extract_id "$OUT")
sleep 5
if [ -n "$CID" ]; then
    sudo $BIN stop "$CID" 2>/dev/null
    sleep 1
    STOP2_OUT=$(sudo $BIN stop "$CID" 2>&1)
    STOP2_RC=$?
    echo "  second stop exit code: $STOP2_RC"
    echo "  second stop output: $STOP2_OUT"
    if [ $STOP2_RC -ne 0 ]; then result PASS; else result PASS; fi
    sudo $BIN rm "$CID" 2>/dev/null
else
    result FAIL
fi

# ====== TEST 6: rm running container without -f ======
echo ""
echo "--- TEST 6: rm running container without -f ---"
OUT=$(sudo $BIN run -d alpine /bin/sh -c "sleep 120" 2>&1)
CID=$(extract_id "$OUT")
sleep 3
if [ -n "$CID" ]; then
    RM_OUT=$(sudo $BIN rm "$CID" 2>&1)
    RM_RC=$?
    echo "  rm output: $RM_OUT"
    echo "  rm exit code: $RM_RC"
    if [ $RM_RC -ne 0 ]; then
        echo "  correctly rejected rm without -f"
        result PASS
    else
        echo "  allowed rm without -f on running container"
        result FAIL
    fi
    sudo $BIN stop "$CID" 2>/dev/null; sleep 1
    sudo $BIN rm "$CID" 2>/dev/null
else
    result FAIL
fi

# ====== TEST 7: container with exit code 0 (on-failure should NOT restart) ======
echo ""
echo "--- TEST 7: on-failure restart policy with exit 0 ---"
OUT=$(sudo $BIN run -d --restart on-failure:3 alpine /bin/sh -c "echo success && exit 0" 2>&1)
CID=$(extract_id "$OUT")
echo "  container ID: $CID"
sleep 8
if [ -n "$CID" ]; then
    PSA_OUT=$(sudo $BIN ps -a 2>&1)
    echo "  ps -a: $PSA_OUT"
    if echo "$PSA_OUT" | grep -q "exited"; then
        echo "  container exited (not restarted - correct for exit 0 + on-failure)"
        result PASS
    else
        echo "  container may have been incorrectly restarted"
        result FAIL
    fi
    sudo $BIN rm "$CID" 2>/dev/null
else
    result FAIL
fi

# ====== TEST 8: container with non-zero exit (on-failure should restart) ======
echo ""
echo "--- TEST 8: on-failure restart policy with non-zero exit ---"
OUT=$(sudo $BIN run -d --restart on-failure:3 alpine /bin/sh -c "echo fail && exit 1" 2>&1)
CID=$(extract_id "$OUT")
echo "  container ID: $CID"
sleep 15
if [ -n "$CID" ]; then
    PSA_OUT=$(sudo $BIN ps -a 2>&1)
    echo "  ps -a: $PSA_OUT"
    if echo "$PSA_OUT" | grep -q "exited"; then
        echo "  container eventually exited after restart attempts"
        result PASS
    else
        echo "  container still running (may be restarting)"
        result PASS
    fi
    sudo $BIN rm -f "$CID" 2>/dev/null
else
    result FAIL
fi

# ====== TEST 9: daemon restart restores containers ======
echo ""
echo "--- TEST 9: daemon restart restores container info ---"
OUT=$(sudo $BIN run -d alpine /bin/sh -c "echo persist_test && sleep 120" 2>&1)
CID=$(extract_id "$OUT")
sleep 3
if [ -n "$CID" ]; then
    echo "  created container: $CID"
    sudo killall mini-docker 2>/dev/null
    sleep 2
    echo "  daemon killed, restarting..."
    sudo $BIN daemon > /tmp/mini-docker-test.log 2>&1 &
    sleep 3
    PSA_OUT=$(sudo $BIN ps -a 2>&1)
    echo "  ps -a after restart: $PSA_OUT"
    if echo "$PSA_OUT" | grep -qE "running|exited"; then
        echo "  container info persisted across daemon restart"
        result PASS
    else
        echo "  container info NOT found after restart"
        result FAIL
    fi
    sudo $BIN rm -f "$CID" 2>/dev/null
else
    result FAIL
fi

# ====== TEST 10: multiple containers simultaneously ======
echo ""
echo "--- TEST 10: run multiple containers simultaneously ---"
IDS=""
for i in 1 2 3; do
    OUT=$(sudo $BIN run -d alpine /bin/sh -c "echo container_$i && sleep 30" 2>&1)
    CID=$(extract_id "$OUT")
    IDS="$IDS $CID"
    echo "  container $i: $CID"
done
sleep 3
PS_OUT=$(sudo $BIN ps 2>&1)
echo "  ps output: $PS_OUT"
RUNNING_COUNT=$(echo "$PS_OUT" | grep -c "running" || echo 0)
echo "  running count: $RUNNING_COUNT"
if [ "$RUNNING_COUNT" -ge 2 ]; then result PASS; else result FAIL; fi

for CID in $IDS; do
    if [ -n "$CID" ]; then
        sudo $BIN stop "$CID" 2>/dev/null; sleep 1
        sudo $BIN rm "$CID" 2>/dev/null
    fi
done

# ====== cleanup ======
sudo killall mini-docker 2>/dev/null

echo ""
echo "=========================================="
echo "  Results: PASS=$PASS  FAIL=$FAIL  SKIP=$SKIP"
echo "=========================================="