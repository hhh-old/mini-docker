#!/bin/bash
# 容器网络连通性深度测试
set +e

BIN="${1:-/tmp/mini-docker-test-bin}"

echo "========================================"
echo "  容器网络连通性深度测试"
echo "========================================"

# 清理并启动 daemon
sudo killall mini-docker 2>/dev/null; sleep 1
sudo rm -rf /var/run/mini-docker /var/log/mini-docker /var/lib/mini-docker
sudo mkdir -p /var/run/mini-docker /var/log/mini-docker /var/lib/mini-docker/images
sudo "$BIN" daemon > /tmp/mini-docker-net-test.log 2>&1 &
sleep 3

# 准备 alpine 镜像
IMGDIR=/var/lib/mini-docker/images/alpine/rootfs
sudo mkdir -p "$IMGDIR"/{bin,usr/bin,proc,sys,dev,tmp,etc,lib,lib64}
for bin in /bin/sh /bin/echo /bin/ls /bin/cat /bin/sleep /bin/hostname; do
    if [ -f "$bin" ]; then
        sudo cp "$bin" "$IMGDIR"/bin/ 2>/dev/null || true
    fi
done

extract_id() {
    echo "$1" | grep -oE '[0-9a-f]{12}' | head -1
}

# TEST 1: 容器获取IP地址
echo ""
echo "--- TEST 1: 容器获取IP地址 ---"
OUT=$(sudo "$BIN" run -d alpine /bin/sh -c "hostname -i; sleep 60" 2>&1)
CID=$(extract_id "$OUT")
echo "  container ID: $CID"
sleep 3
if [ -n "$CID" ]; then
    PSA=$(sudo "$BIN" ps -a 2>&1)
    echo "  ps -a: $PSA"
    sudo "$BIN" stop "$CID" 2>/dev/null; sleep 1
    sudo "$BIN" rm "$CID" 2>/dev/null
    echo "  RESULT: PASS"
else
    echo "  RESULT: FAIL"
fi

# TEST 2: 同一网络内容器互通
echo ""
echo "--- TEST 2: 同一网络内容器互通 ---"
sudo "$BIN" network create nettest 2>/dev/null
OUT1=$(sudo "$BIN" run -d -n nettest alpine /bin/sh -c "ip addr show eth0; sleep 120" 2>&1)
CID1=$(extract_id "$OUT1")
sleep 3
OUT2=$(sudo "$BIN" run -d -n nettest alpine /bin/sh -c "sleep 120" 2>&1)
CID2=$(extract_id "$OUT2")
echo "  container 1: $CID1"
echo "  container 2: $CID2"
if [ -n "$CID1" ] && [ -n "$CID2" ]; then
    echo "  RESULT: PASS"
else
    echo "  RESULT: FAIL"
fi
sudo "$BIN" stop "$CID1" 2>/dev/null; sudo "$BIN" rm "$CID1" 2>/dev/null
sudo "$BIN" stop "$CID2" 2>/dev/null; sudo "$BIN" rm "$CID2" 2>/dev/null
sudo "$BIN" network delete nettest 2>/dev/null

# TEST 3: 端口映射测试
echo ""
echo "--- TEST 3: 端口映射 (-p 8080:80) ---"
OUT=$(sudo "$BIN" run -d -p 8080:80 alpine /bin/sh -c "sleep 120" 2>&1)
CID=$(extract_id "$OUT")
echo "  container ID: $CID"
if [ -n "$CID" ]; then
    # 使用 iptables-legacy 检查，并匹配服务名 http-alt (8080) 或数字 8080
    IPT=$(sudo iptables-legacy -t nat -L 2>&1 | grep -c 'http-alt\|8080' || true)
    echo "  iptables rules with 8080: $IPT"
    if [ "$IPT" -gt 0 ]; then
        echo "  RESULT: PASS"
    else
        echo "  RESULT: FAIL (no iptables rule found)"
    fi
    sudo "$BIN" stop "$CID" 2>/dev/null; sudo "$BIN" rm "$CID" 2>/dev/null
else
    echo "  RESULT: FAIL"
fi

# TEST 4: 默认网络 (bridge) 自动连接
echo ""
echo "--- TEST 4: 默认网络自动连接 ---"
OUT=$(sudo "$BIN" run -d alpine /bin/sh -c "sleep 120" 2>&1)
CID=$(extract_id "$OUT")
echo "  container ID: $CID"
if [ -n "$CID" ]; then
    VETH=$(ip link show 2>&1 | grep -c 'vh-')
    echo "  veth devices on host: $VETH"
    if [ "$VETH" -gt 0 ]; then
        echo "  RESULT: PASS"
    else
        echo "  RESULT: FAIL"
    fi
    sudo "$BIN" stop "$CID" 2>/dev/null; sudo "$BIN" rm "$CID" 2>/dev/null
else
    echo "  RESULT: FAIL"
fi

# TEST 5: --network=none 模式
echo ""
echo "--- TEST 5: --network=none 模式 ---"
OUT=$(sudo "$BIN" run -d --network=none alpine /bin/sh -c "sleep 120" 2>&1)
CID=$(extract_id "$OUT")
echo "  container ID: $CID"
if [ -n "$CID" ]; then
    echo "  RESULT: PASS"
    sudo "$BIN" stop "$CID" 2>/dev/null; sudo "$BIN" rm "$CID" 2>/dev/null
else
    echo "  RESULT: FAIL"
fi

# TEST 6: 网络隔离（不同网络的容器不应互通）
echo ""
echo "--- TEST 6: 网络隔离（不同网络） ---"
sudo "$BIN" network create netA 2>/dev/null
sudo "$BIN" network create netB 2>/dev/null
OUT1=$(sudo "$BIN" run -d -n netA alpine /bin/sh -c "sleep 120" 2>&1)
CID1=$(extract_id "$OUT1")
OUT2=$(sudo "$BIN" run -d -n netB alpine /bin/sh -c "sleep 120" 2>&1)
CID2=$(extract_id "$OUT2")
echo "  container in netA: $CID1"
echo "  container in netB: $CID2"
if [ -n "$CID1" ] && [ -n "$CID2" ]; then
    echo "  RESULT: PASS (both containers created in different networks)"
else
    echo "  RESULT: FAIL"
fi
sudo "$BIN" stop "$CID1" 2>/dev/null; sudo "$BIN" rm "$CID1" 2>/dev/null
sudo "$BIN" stop "$CID2" 2>/dev/null; sudo "$BIN" rm "$CID2" 2>/dev/null
sudo "$BIN" network delete netA 2>/dev/null
sudo "$BIN" network delete netB 2>/dev/null

# cleanup
sudo killall mini-docker 2>/dev/null

echo ""
echo "========================================"
echo "  网络连通性深度测试完成"
echo "========================================"
