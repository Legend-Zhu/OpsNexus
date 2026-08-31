# OpsGaurdWeb 管理端一体化镜像（Go 后端 + 前端 dist 静态托管 + 内嵌镜像仓库）。
#
# 构建上下文为 OpsGaurdWeb/ 目录：
#   cd OpsGaurdWeb
#   docker build -t opsguard-server:1.0.0 -f deploy/Dockerfile .
#
# 前端产物直接使用已构建的 web/dist（先在本地 npm run build）。
# 运行：
#   docker run -d --name opsguard-server -p 8080:8080 \
#     -v /opt/opsguard/server/data:/app/data \
#     -v /var/run/docker.sock:/var/run/docker.sock \
#     opsguard-server:1.0.0

# ---- 构建阶段 ----
FROM golang:1.25-alpine AS build
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/server ./cmd/server

# ---- 运行阶段 ----
FROM alpine:3.20
# ca-certificates/tzdata: TLS 与时区；docker-cli: 页面传包构建走挂载进来的
# 宿主 docker.sock（registry.enabled 时管理端用 docker CLI 构建并 push 本机仓库）。
RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.aliyun.com/g' /etc/apk/repositories && \
    apk add --no-cache ca-certificates tzdata docker-cli
# 容器时区固定为北京时间：patrol cron 等 time.Local 语义随之正确
#（tzdata 已装；配合 patrol.timezone 显式绑定，双保险）
ENV TZ=Asia/Shanghai
WORKDIR /app
COPY --from=build /out/server /app/server
# 前端静态托管目录固定为工作目录下的 ./web（router.NoRoute 回退 index.html）
COPY web/dist /app/web
# 默认配置；生产以 -v 挂载 /app/configs/config.yaml 覆盖
COPY deploy/config.docker.yaml /app/configs/config.yaml
VOLUME ["/app/data"]
EXPOSE 8080
ENTRYPOINT ["/app/server", "-config", "configs/config.yaml"]
