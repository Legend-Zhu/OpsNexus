#!/bin/bash
# OpsGaurd 离线安装 Docker（UOS Server 20 / x86_64）
# 用法：把 docker-<ver>.tgz 静态包与本脚本放同一目录，然后：
#   bash install-docker.sh docker-27.5.1.tgz
set -euo pipefail

TGZ="${1:-}"
if [ -z "$TGZ" ] || [ ! -f "$TGZ" ]; then
  echo "usage: bash install-docker.sh <docker-static-tgz>" >&2
  exit 1
fi

echo "==> 解压 $TGZ 到 /usr/local/bin"
tar -xzf "$TGZ" -C /tmp
cp -f /tmp/docker/* /usr/local/bin/
rm -rf /tmp/docker

echo "==> 安装 systemd 单元与 daemon.json"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cp -f "$SCRIPT_DIR/containerd.service" /etc/systemd/system/containerd.service
cp -f "$SCRIPT_DIR/docker.service" /etc/systemd/system/docker.service
mkdir -p /etc/docker
cp -f "$SCRIPT_DIR/daemon.json" /etc/docker/daemon.json

echo "==> 启动 containerd 与 dockerd"
systemctl daemon-reload
systemctl enable --now containerd
systemctl enable --now docker

echo "==> 初始化单节点 swarm（已初始化则跳过）"
docker swarm init 2>/dev/null || echo "swarm 已存在，跳过"

docker version
echo "OK: docker 安装完成"
