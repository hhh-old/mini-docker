#!/bin/bash
# 准备最小 rootfs 的辅助脚本
# 供测试脚本 source 使用
#
# 用法: source prepare-rootfs.sh
# 调用: prepare_rootfs /path/to/rootfs

prepare_rootfs() {
    local ROOTFS="$1"
    if [ -z "$ROOTFS" ]; then
        echo "ERROR: prepare_rootfs requires rootfs path argument"
        return 1
    fi

    sudo rm -rf "$ROOTFS"
    sudo mkdir -p "$ROOTFS"/{bin,usr/bin,proc,sys,dev,tmp,etc,lib,lib64}

    local BINS="/bin/sh /bin/echo /bin/ls /bin/cat /bin/sleep /bin/mkdir /bin/rm"
    for bin in $BINS; do
        if [ -f "$bin" ]; then
            sudo cp "$bin" "$ROOTFS/bin/" 2>/dev/null || true
            for lib in $(ldd "$bin" 2>/dev/null | grep -o '/lib[^ ]*'); do
                local dir=$(dirname "$lib")
                sudo mkdir -p "$ROOTFS$dir" 2>/dev/null || true
                sudo cp "$lib" "$ROOTFS$lib" 2>/dev/null || true
            done
        fi
    done

    echo "rootfs prepared at $ROOTFS: $(ls "$ROOTFS/bin/" 2>/dev/null | tr '\n' ' ')"
}