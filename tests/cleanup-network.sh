#!/bin/bash
# 清理残留的 mini-docker 网桥
set +e
sudo killall mini-docker 2>/dev/null
sleep 1

for br in $(ip link show | grep -oE 'mini-[a-zA-Z0-9]+' | sort -u); do
    if [ "$br" != "mini-bridge" ]; then
        sudo ip link delete "$br" 2>/dev/null
        echo "deleted $br"
    fi
done

# 清理残留的 veth
for veth in $(ip link show | grep -oE 'vh-[a-zA-Z0-9]+' | sort -u); do
    sudo ip link delete "$veth" 2>/dev/null
    echo "deleted $veth"
done

echo "cleanup done"
