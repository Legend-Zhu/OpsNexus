# 可选的容器化部署（与 deploy.sh 的 systemd 裸跑二选一）。
# 182.57 有外网，可直接 docker compose up -d --build。
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY main.go go.mod ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /out/internet-proxy .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /out/internet-proxy /usr/local/bin/internet-proxy
ENTRYPOINT ["internet-proxy"]
