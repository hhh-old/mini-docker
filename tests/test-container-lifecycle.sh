#!/bin/bash
# 容器生命周期与操作测试
# 覆盖: run -d, exec, start, pause/unpause, logs, --name, --restart
# 需要在 WSL2/Linux 中以 root 运行
#
# 用法: sudo bash test-container-lifecycle.sh [/path/to/mini-docker-binary]

set +e

BIN="${1:-/tmp/mini-docker}"
CONTAINERS=""

PASS=0; FAIL=0; SKIP=0
result() {
    if [ "$1" = "PASS" ]; then PASS=$((PASS+1)); elif [ "$1" = "FAIL" ]; then FAIL=$((FAIL+1)); else SKIP=$((SKIP+1)); fi
    echo "  RESULT: $1"
}

echo "=========================================="
echo "  Container Lifecycle & Operations Test"
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
for bin in /bin/sh /bin/echo /bin/ls /bin/cat /bin/sleep /bin/mkdir /bin/rm /bin/hostname; do
    if [ -f "$bin" ]; then
        sudo cp "$bin" $IMGDIR/bin/ 2>/dev/null || true
        for lib in $(ldd $bin 2>/dev/null | grep -o '/lib[^ ]*'); do
            dir=$(dirname "$lib")
            sudo mkdir -p "$IMGDIR$dir" 2>/dev/null || true
            sudo cp "$lib" "$IMGDIR$lib" 2>/dev/null || true
        done
    fi
done

# helper: extract container ID from run output
extract_id() {
    echo "$1" | grep -oP '"id":\s*"\K[^"]+' | head -1
}

# ====== TEST 1: run --name ======
echo "--- TEST 1: run --name custom name ---"
OUT=$(sudo $BIN run -d --name mycontainer alpine /bin/sh -c "echo named_container && sleep 60" 2>&1)
CID=$(extract_id "$OUT")
echo "  container ID: $CID"
if [ -n "$CID" ]; then
    PS_OUT=$(sudo $BIN ps 2>&1)
    if echo "$PS_OUT" | grep -q "mycontainer"; then
        echo "  name found in ps output"
        result PASS
    else
        echo "  name NOT found in ps output"
        echo "  ps: $PS_OUT"
        result FAIL
    fi
    sudo $BIN stop "$CID" 2>/dev/null; sleep 1
    sudo $BIN rm "$CID" 2>/dev/null
else
    echo "  run failed: $(echo "$OUT" | head -3)"
    result FAIL
fi

# ====== TEST 2: exec ======
echo ""
echo "--- TEST 2: exec command in running container ---"
OUT=$(sudo $BIN run -d alpine /bin/sh -c "echo running && sleep 60" 2>&1)
CID=$(extract_id "$OUT")
sleep 3
if [ -n "$CID" ]; then
    EXEC_OUT=$(sudo $BIN exec "$CID" echo "exec_works" 2>&1)
    echo "  exec output: $EXEC_OUT"
    if echo "$EXEC_OUT" | grep -q "exec_works"; then
        result PASS
    else
        result FAIL
    fi
    sudo $BIN stop "$CID" 2>/dev/null; sleep 1
    sudo $BIN rm "$CID" 2>/dev/null
else
    result FAIL
fi

# ====== TEST 3: logs ======
echo ""
echo "--- TEST 3: container logs ---"
OUT=$(sudo $BIN run -d alpine /bin/sh -c "echo LOG_LINE_1 && echo LOG_LINE_2 && sleep 60" 2>&1)
CID=$(extract_id "$OUT")
sleep 3
if [ -n "$CID" ]; then
    LOGS_OUT=$(sudo $BIN logs "$CID" 2>&1)
    echo "  logs output: $LOGS_OUT"
    if echo "$LOGS_OUT" | grep -q "LOG_LINE_1"; then
        result PASS
    else
        result FAIL
    fi
    sudo $BIN stop "$CID" 2>/dev/null; sleep 1
    sudo $BIN rm "$CID" 2>/dev/null
else
    result FAIL
fi

# ====== TEST 4: start (restart stopped container) ======
echo ""
echo "--- TEST 4: start stopped container ---"
OUT=$(sudo $BIN run -d alpine /bin/sh -c "echo first_start && sleep 60" 2>&1)
CID=$(extract_id "$OUT")
sleep 3
if [ -n "$CID" ]; then
    sudo $BIN stop "$CID" 2>/dev/null
    sleep 2
    START_OUT=$(sudo $BIN start "$CID" 2>&1)
    START_RC=$?
    echo "  start output: $START_OUT"
    echo "  start exit code: $START_RC"
    sleep 2
    PS_OUT=$(sudo $BIN ps 2>&1)
    if echo "$PS_OUT" | grep -q "running"; then
        result PASS
    else
        result FAIL
    fi
    sudo $BIN stop "$CID" 2>/dev/null; sleep 1
    sudo $BIN rm "$CID" 2>/dev/null
else
    result FAIL
fi

# ====== TEST 5: pause / unpause ======
echo ""
echo "--- TEST 5: pause and unpause container ---"
OUT=$(sudo $BIN run -d alpine /bin/sh -c "echo paused_test && sleep 120" 2>&1)
CID=$(extract_id "$OUT")
sleep 3
if [ -n "$CID" ]; then
    PAUSE_OUT=$(sudo $BIN pause "$CID" 2>&1)
    PAUSE_RC=$?
    echo "  pause output: $PAUSE_OUT"
    echo "  pause exit code: $PAUSE_RC"
    sleep 1
    if [ $PAUSE_RC -eq 0 ]; then
        PSA_OUT=$(sudo $BIN ps -a 2>&1)
        if echo "$PSA_OUT" | grep -qi "paused"; then
            echo "  container status: paused"
        else
            echo "  container status: $(echo "$PSA_OUT" | grep "$CID" || echo 'not found')"
        fi
        UNPAUSE_OUT=$(sudo $BIN unpause "$CID" 2>&1)
        UNPAUSE_RC=$?
        echo "  unpause exit code: $UNPAUSE_RC"
        if [ $UNPAUSE_RC -eq 0 ]; then result PASS; else result FAIL; fi
    else
        echo "  pause failed"
        result FAIL
    fi
    sudo $BIN stop "$CID" 2>/dev/null; sleep 1
    sudo $BIN rm "$CID" 2>/dev/null
else
    result FAIL
fi

# ====== TEST 6: rm -f (force remove running container) ======
echo ""
echo "--- TEST 6: rm -f force remove running container ---"
OUT=$(sudo $BIN run -d alpine /bin/sh -c "sleep 120" 2>&1)
CID=$(extract_id "$OUT")
sleep 3
if [ -n "$CID" ]; then
    RM_OUT=$(sudo $BIN rm -f "$CID" 2>&1)
    RM_RC=$?
    echo "  rm -f output: $RM_OUT"
    echo "  rm -f exit code: $RM_RC"
    sleep 1
    PSA_OUT=$(sudo $BIN ps -a 2>&1)
    if ! echo "$PSA_OUT" | grep -q "$CID"; then
        result PASS
    else
        result FAIL
    fi
else
    result FAIL
fi

# ====== TEST 7: short ID operations ======
echo ""
echo "--- TEST 7: short ID prefix matching ---"
OUT=$(sudo $BIN run -d alpine /bin/sh -c "echo shortid_test && sleep 60" 2>&1)
CID=$(extract_id "$OUT")
sleep 3
if [ -n "$CID" ] && [ ${#CID} -ge 12 ]; then
    SHORT_ID="${CID:0:8}"
    echo "  full ID:  $CID"
    echo "  short ID: $SHORT_ID"
    STOP_OUT=$(sudo $BIN stop "$SHORT_ID" 2>&1)
    STOP_RC=$?
    echo "  stop with short ID exit code: $STOP_RC"
    sleep 1
    if [ $STOP_RC -eq 0 ]; then
        result PASS
    else
        echo "  stop output: $STOP_OUT"
        result FAIL
    fi
    sudo $BIN rm "$CID" 2>/dev/null
else
    result FAIL
fi

# ====== cleanup ======
sudo killall mini-docker 2>/dev/null

echo ""
echo "=========================================="
echo "  Results: PASS=$PASS  FAIL=$FAIL  SKIP=$SKIP"
echo "=========================================="