#!/usr/bin/env bash
# 在互联网服务器（10.50.182.57）root 下执行。前置：当前目录有
#   internet-proxy        （linux-amd64 二进制）
#   proxy.env             （运行配置，含凭据）
# 安装为 systemd 服务 opsguard-internet-proxy 并启动。
set -euo pipefail

INSTALL_DIR=/opt/opsguard/internet-proxy
ENV_FILE=/etc/opsguard-internet-proxy.env

command -v systemctl >/dev/null || { echo "需要 systemd"; exit 1; }
[ -f internet-proxy ] || { echo "缺少二进制 internet-proxy"; exit 1; }
[ -f proxy.env ] || { echo "缺少配置 proxy.env"; exit 1; }

mkdir -p "$INSTALL_DIR"
install -m 0755 internet-proxy "$INSTALL_DIR/internet-proxy"
install -m 0600 proxy.env "$ENV_FILE"

cat > /etc/systemd/system/opsguard-internet-proxy.service <<'EOF'
[Unit]
Description=OpsGaurd internet proxy (LLM reverse proxy + Feishu notify relay)
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/opsguard-internet-proxy.env
ExecStart=/opt/opsguard/internet-proxy/internet-proxy
Restart=always
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now opsguard-internet-proxy
sleep 1
systemctl --no-pager -l status opsguard-internet-proxy | head -12

echo
echo "已安装。验证："
echo "  curl http://10.50.182.57:7072/healthz"
echo "  /opt/opsguard/internet-proxy/internet-proxy -selftest         # 群内收两条自测消息"
echo "  /opt/opsguard/internet-proxy/internet-proxy -selftest-urgent  # 额外加急值班用户（慎用）"
