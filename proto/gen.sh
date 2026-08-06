#!/usr/bin/env bash
# Generate Go gRPC stubs from proto/opsguard.proto into BOTH modules.
# The same .proto compiles into two independent package paths so server and
# worker do not cross-import:
#   - Worker (server side):    gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/grpcapi/pb
#   - OpsGaurdWeb (client):    gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb
#
# Requirements: protoc, protoc-gen-go, protoc-gen-go-grpc on PATH.
# Run from the repo root: bash proto/gen.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PROTO_DIR="$REPO_ROOT/proto"
PROTO_FILE="opsguard.proto"

# Locate protoc well-known types (timestamp.proto). Bundled under protoc's
# include/ when installed; fall back to the repo copy if needed.
PROTOC_INCLUDE=""
if command -v protoc >/dev/null 2>&1; then
  PROTOC_BIN="$(command -v protoc)"
  PROTOC_INCLUDE="$(dirname "$PROTOC_BIN")/../include"
fi

echo "==> generating Worker stubs (gitee.com/.../Worker/internal/grpcapi/pb)"
WORKER_OUT="$REPO_ROOT/Worker/internal/grpcapi/pb"
protoc \
  -I "$PROTO_DIR" \
  ${PROTOC_INCLUDE:+-I "$PROTOC_INCLUDE"} \
  --go_out="$WORKER_OUT" \
  --go_opt="M${PROTO_FILE}=gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/grpcapi/pb" \
  --go_opt=paths=source_relative \
  --go-grpc_out="$WORKER_OUT" \
  --go-grpc_opt="M${PROTO_FILE}=gitee.com/legeosoft_legendzhu/OpsGaurd/Worker/internal/grpcapi/pb" \
  --go-grpc_opt=paths=source_relative \
  "$PROTO_DIR/$PROTO_FILE"

echo "==> generating OpsGaurdWeb stubs (gitee.com/.../OpsGaurdWeb/server/internal/workerproxy/pb)"
WEB_OUT="$REPO_ROOT/OpsGaurdWeb/server/internal/workerproxy/pb"
protoc \
  -I "$PROTO_DIR" \
  ${PROTOC_INCLUDE:+-I "$PROTOC_INCLUDE"} \
  --go_out="$WEB_OUT" \
  --go_opt="M${PROTO_FILE}=gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb" \
  --go_opt=paths=source_relative \
  --go-grpc_out="$WEB_OUT" \
  --go-grpc_opt="M${PROTO_FILE}=gitee.com/legeosoft_legendzhu/OpsGaurd/OpsGaurdWeb/server/internal/workerproxy/pb" \
  --go-grpc_opt=paths=source_relative \
  "$PROTO_DIR/$PROTO_FILE"

echo "==> done"
