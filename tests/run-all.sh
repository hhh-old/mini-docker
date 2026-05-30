#!/bin/bash
# mini-docker 一键全量测试入口
# 编译 -> runtime 测试 -> daemon 测试 -> 容器生命周期 -> 网络/卷/资源 -> 边界场景
#
# 用法:
#   在 WSL2/Linux 中:  sudo bash run-all.sh
#   从 Windows WSL:    wsl bash -c "cd /mnt/d/.../tests && sudo bash run-all.sh"

set +e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
BIN="/tmp/mini-docker-test-bin"

echo "============================================"
echo "  mini-docker Full Test Suite"
echo "============================================"
echo "Project: $PROJECT_DIR"
echo "Date:    $(date)"
echo ""

# ---- Step 0: check Go ----
echo "--- Step 0: check Go environment ---"
GO_PATH=$(which go 2>/dev/null || echo "/usr/local/go/bin/go")
if [ ! -x "$GO_PATH" ]; then
    GO_PATH="/usr/local/go/bin/go"
fi
if [ -x "$GO_PATH" ]; then
    GO_VER=$($GO_PATH version 2>&1)
    echo "  $GO_VER"
else
    echo "  ERROR: Go not found. Install Go 1.23+ and add to PATH."
    exit 1
fi

# ---- Step 1: compile ----
echo ""
echo "--- Step 1: compile ---"
export PATH="$(dirname $GO_PATH):$PATH"
cd "$PROJECT_DIR"
go build -buildvcs=false -o "$BIN" . 2>&1
BUILD_RC=$?
if [ $BUILD_RC -ne 0 ]; then
    echo "  ERROR: build failed"
    exit 1
fi
echo "  compiled: $BIN ($(du -h $BIN | cut -f1))"

# ---- run all test suites ----
RC_SUM=0

run_suite() {
    local name="$1"
    local script="$2"
    echo ""
    echo "=========================================="
    echo "  $name"
    echo "=========================================="
    bash "$SCRIPT_DIR/$script" "$BIN"
    local rc=$?
    RC_SUM=$((RC_SUM + rc))
    return $rc
}

run_suite "Part 1: Runtime Layer Test"         "test-runtime.sh"
run_suite "Part 2: Daemon Integration Test"    "test-daemon.sh"
run_suite "Part 3: Container Lifecycle Test"   "test-container-lifecycle.sh"
run_suite "Part 4: Network/Volume/Resource Test" "test-network-volume-resource.sh"
run_suite "Part 5: Edge Cases & Error Handling"  "test-edge-cases.sh"

# ---- summary ----
echo ""
echo "============================================"
echo "  Full Test Suite Complete"
echo "============================================"
echo ""

if [ $RC_SUM -eq 0 ]; then
    echo "  *** ALL TESTS PASSED ***"
else
    echo "  *** SOME TESTS FAILED ***"
    echo "  Total non-zero exit codes: $RC_SUM"
fi

# cleanup
rm -f "$BIN" 2>/dev/null