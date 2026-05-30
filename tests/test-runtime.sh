#!/bin/bash
# runtime 层低级 API 测试
# 直接测试 runtime create/start，不依赖 Daemon
# 需要在 WSL2/Linux 中以 root 运行
#
# 用法: sudo bash test-runtime.sh [/path/to/mini-docker-binary]

set +e

BIN="${1:-/tmp/mini-docker}"
ROOTFS=/tmp/mini-docker-test-rootfs
BUNDLE=/tmp/mini-docker-test-bundle
CONTAINER_ID="testRuntime"

PASS=0
FAIL=0
SKIP=0

result() {
    if [ "$1" = "PASS" ]; then PASS=$((PASS+1)); elif [ "$1" = "FAIL" ]; then FAIL=$((FAIL+1)); else SKIP=$((SKIP+1)); fi
    echo "  RESULT: $1"
}

echo "=========================================="
echo "  mini-docker Runtime Layer Test"
echo "=========================================="
echo "Date:   $(date)"
echo "Binary: $BIN"
echo ""

if [ ! -f "$BIN" ]; then
    echo "ERROR: binary not found: $BIN"
    exit 1
fi

# ---- prepare rootfs ----
echo "--- Prepare minimal rootfs ---"
sudo rm -rf $ROOTFS
sudo mkdir -p $ROOTFS/{bin,usr/bin,proc,sys,dev,tmp,etc,lib,lib64}
for bin in /bin/sh /bin/echo /bin/ls /bin/cat /bin/sleep; do
    if [ -f "$bin" ]; then
        sudo cp "$bin" $ROOTFS/bin/ 2>/dev/null || true
        for lib in $(ldd $bin 2>/dev/null | grep -o '/lib[^ ]*'); do
            dir=$(dirname "$lib")
            sudo mkdir -p "$ROOTFS$dir" 2>/dev/null || true
            sudo cp "$lib" "$ROOTFS$lib" 2>/dev/null || true
        done
    fi
done
echo "  bin contents: $(ls $ROOTFS/bin/ 2>/dev/null | tr '\n' ' ')"

# ---- cleanup old state ----
sudo rm -rf /var/lib/mini-docker/runtime/$CONTAINER_ID 2>/dev/null
sudo rm -f $BUNDLE/start-fifo 2>/dev/null
sudo rm -rf $BUNDLE
sudo mkdir -p $BUNDLE

# ---- prepare OCI bundle ----
sudo tee $BUNDLE/config.json > /dev/null << 'OCEOF'
{
  "ociVersion": "1.0.0",
  "root": {"path": "/tmp/mini-docker-test-rootfs", "readonly": false},
  "process": {
    "terminal": false,
    "args": ["/bin/sh", "-c", "echo HELLO_FROM_CONTAINER"],
    "env": ["PATH=/usr/bin:/bin"]
  },
  "hostname": "test-container",
  "linux": {
    "namespaces": [
      {"type": "pid"},
      {"type": "uts"},
      {"type": "mount"}
    ]
  }
}
OCEOF

# ====== TEST 1: runtime create ======
echo ""
echo "--- TEST 1: runtime create ---"
CREATE_OUT=$(sudo $BIN runtime create $CONTAINER_ID --bundle $BUNDLE 2>&1)
CREATE_RC=$?
echo "  output: $CREATE_OUT"
echo "  exit code: $CREATE_RC"
if [ $CREATE_RC -eq 0 ]; then result PASS; else result FAIL; fi

# ====== TEST 2: init process alive ======
echo ""
echo "--- TEST 2: init process alive after create ---"
STATE_FILE=/var/lib/mini-docker/runtime/$CONTAINER_ID/state.json
PID=""
if [ -f $STATE_FILE ]; then
    PID=$(sudo cat $STATE_FILE | python3 -c "import sys,json;print(json.load(sys.stdin)['pid'])" 2>/dev/null || true)
    if [ -z "$PID" ]; then
        PID=$(echo "$CREATE_OUT" | tail -1 | tr -d '[:space:]')
    fi
    echo "  PID: $PID"
    if [ -n "$PID" ] && sudo kill -0 $PID 2>/dev/null; then
        echo "  process: ALIVE"
        result PASS
    else
        echo "  process: DEAD"
        result FAIL
    fi
else
    echo "  state.json not found"
    result FAIL
fi

# ====== TEST 3: runtime start ======
echo ""
echo "--- TEST 3: runtime start ---"
if [ -n "$PID" ] && sudo kill -0 $PID 2>/dev/null; then
    START_OUT=$(sudo $BIN runtime start $CONTAINER_ID 2>&1)
    START_RC=$?
    echo "  output: $START_OUT"
    echo "  exit code: $START_RC"
    sleep 2
    if [ $START_RC -eq 0 ]; then result PASS; else result FAIL; fi
else
    echo "  SKIP - init process not alive"
    result SKIP
fi

# ====== TEST 4: process exited after start ======
echo ""
echo "--- TEST 4: process exited after start ---"
if [ -n "$PID" ]; then
    if sudo kill -0 $PID 2>/dev/null; then
        echo "  process: still ALIVE - command may still be running or hung"
        result FAIL
    else
        echo "  process: DEAD - exited as expected after echo command"
        result PASS
    fi
else
    result SKIP
fi

# ---- cleanup ----
sudo rm -rf /var/lib/mini-docker/runtime/$CONTAINER_ID 2>/dev/null
sudo rm -rf $ROOTFS $BUNDLE 2>/dev/null

echo ""
echo "=========================================="
echo "  Results: PASS=$PASS  FAIL=$FAIL  SKIP=$SKIP"
echo "=========================================="