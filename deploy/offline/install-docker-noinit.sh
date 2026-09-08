#!/bin/bash
# OpsGaurd 离线安装 Docker —— 安责险集群 worker 节点版（不执行 swarm init，
# 装完由人工 docker swarm join 到 manager 10.60.185.66）
# 用法：bash install-docker-noinit.sh docker-27.5.1.tgz
set -euo pipefail

TGZ="${1:-}"
if [ -z "$TGZ" ] || [ ! -f "$TGZ" ]; then
  echo "usage: bash install-docker-noinit.sh <docker-static-tgz>" >&2
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

docker version
echo "OK: docker 安装完成（未 init swarm，稍后 join 10.60.185.66）"
