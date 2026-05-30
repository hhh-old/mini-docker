#!/bin/bash
# 网络、数据卷、资源限制、镜像管理测试
# 覆盖: network create/list/delete, volume create/list/rm, run -m/-c, images
# 需要在 WSL2/Linux 中以 root 运行
#
# 用法: sudo bash test-network-volume-resource.sh [/path/to/mini-docker-binary]

set +e

BIN="${1:-/tmp/mini-docker}"

PASS=0; FAIL=0; SKIP=0
result() {
    if [ "$1" = "PASS" ]; then PASS=$((PASS+1)); elif [ "$1" = "FAIL" ]; then FAIL=$((FAIL+1)); else SKIP=$((SKIP+1)); fi
    echo "  RESULT: $1"
}

echo "=========================================="
echo "  Network, Volume & Resource Test"
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

extract_id() { echo "$1" | grep -oP '"id":\s*"\K[^"]+' | head -1; }

# ====== TEST 1: network create ======
echo "--- TEST 1: network create ---"
NET_OUT=$(sudo $BIN network create testnet 2>&1)
NET_RC=$?
echo "  output: $NET_OUT"
echo "  exit code: $NET_RC"
if [ $NET_RC -eq 0 ]; then result PASS; else result FAIL; fi

# ====== TEST 2: network list ======
echo ""
echo "--- TEST 2: network list ---"
NETLIST_OUT=$(sudo $BIN network list 2>&1)
echo "  output: $NETLIST_OUT"
if echo "$NETLIST_OUT" | grep -q "testnet"; then result PASS; else result FAIL; fi

# ====== TEST 3: network delete ======
echo ""
echo "--- TEST 3: network delete ---"
NETDEL_OUT=$(sudo $BIN network delete testnet 2>&1)
NETDEL_RC=$?
echo "  output: $NETDEL_OUT"
echo "  exit code: $NETDEL_RC"
if [ $NETDEL_RC -eq 0 ]; then result PASS; else result FAIL; fi

# ====== TEST 4: volume create ======
echo ""
echo "--- TEST 4: volume create ---"
VOL_OUT=$(sudo $BIN volume create myvol 2>&1)
VOL_RC=$?
echo "  output: $VOL_OUT"
echo "  exit code: $VOL_RC"
if [ $VOL_RC -eq 0 ]; then result PASS; else result FAIL; fi

# ====== TEST 5: volume list ======
echo ""
echo "--- TEST 5: volume list ---"
VOLLIST_OUT=$(sudo $BIN volume list 2>&1)
echo "  output: $VOLLIST_OUT"
if echo "$VOLLIST_OUT" | grep -q "myvol"; then result PASS; else result FAIL; fi

# ====== TEST 6: volume rm ======
echo ""
echo "--- TEST 6: volume rm ---"
VOLRM_OUT=$(sudo $BIN volume rm myvol 2>&1)
VOLRM_RC=$?
echo "  output: $VOLRM_OUT"
echo "  exit code: $VOLRM_RC"
if [ $VOLRM_RC -eq 0 ]; then result PASS; else result FAIL; fi

# ====== TEST 7: images list ======
echo ""
echo "--- TEST 7: images list ---"
IMGS_OUT=$(sudo $BIN images 2>&1)
echo "  output: $IMGS_OUT"
if echo "$IMGS_OUT" | grep -q "alpine"; then result PASS; else result FAIL; fi

# ====== TEST 8: run with memory limit ======
echo ""
echo "--- TEST 8: run with -m memory limit ---"
OUT=$(sudo $BIN run -d -m 64m alpine /bin/sh -c "echo mem_test && sleep 10" 2>&1)
CID=$(extract_id "$OUT")
echo "  container ID: $CID"
sleep 3
if [ -n "$CID" ]; then
    PSA_OUT=$(sudo $BIN ps -a 2>&1)
    if echo "$PSA_OUT" | grep -q "running\|exited"; then
        echo "  container ran with memory limit"
        result PASS
    else
        echo "  container did not run properly"
        result FAIL
    fi
    sudo $BIN stop "$CID" 2>/dev/null; sleep 1
    sudo $BIN rm "$CID" 2>/dev/null
else
    echo "  run failed: $(echo "$OUT" | head -3)"
    result FAIL
fi

# ====== TEST 9: run with cpu shares ======
echo ""
echo "--- TEST 9: run with -c cpu shares ---"
OUT=$(sudo $BIN run -d -c 512 alpine /bin/sh -c "echo cpu_test && sleep 10" 2>&1)
CID=$(extract_id "$OUT")
echo "  container ID: $CID"
sleep 3
if [ -n "$CID" ]; then
    echo "  container ran with cpu shares"
    result PASS
    sudo $BIN stop "$CID" 2>/dev/null; sleep 1
    sudo $BIN rm "$CID" 2>/dev/null
else
    result FAIL
fi

# ====== TEST 10: run with volume mount ======
echo ""
echo "--- TEST 10: run with -v volume mount ---"
sudo $BIN volume create testvol 2>/dev/null
OUT=$(sudo $BIN run -d -v testvol:/data alpine /bin/sh -c "echo vol_mount && sleep 10" 2>&1)
CID=$(extract_id "$OUT")
echo "  container ID: $CID"
sleep 3
if [ -n "$CID" ]; then
    echo "  container ran with volume mount"
    result PASS
    sudo $BIN stop "$CID" 2>/dev/null; sleep 1
    sudo $BIN rm "$CID" 2>/dev/null
else
    echo "  run failed: $(echo "$OUT" | head -3)"
    result FAIL
fi
sudo $BIN volume rm testvol 2>/dev/null

# ====== TEST 11: run with network ======
echo ""
echo "--- TEST 11: run with -n network ---"
sudo $BIN network create testnet2 2>/dev/null
OUT=$(sudo $BIN run -d -n testnet2 alpine /bin/sh -c "echo net_test && sleep 10" 2>&1)
CID=$(extract_id "$OUT")
echo "  container ID: $CID"
sleep 3
if [ -n "$CID" ]; then
    echo "  container ran with custom network"
    result PASS
    sudo $BIN stop "$CID" 2>/dev/null; sleep 1
    sudo $BIN rm "$CID" 2>/dev/null
else
    echo "  run failed: $(echo "$OUT" | head -3)"
    result FAIL
fi
sudo $BIN network delete testnet2 2>/dev/null

# ====== cleanup ======
sudo killall mini-docker 2>/dev/null

echo ""
echo "=========================================="
echo "  Results: PASS=$PASS  FAIL=$FAIL  SKIP=$SKIP"
echo "=========================================="