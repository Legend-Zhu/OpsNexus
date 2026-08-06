# Multi-stage build for the OpsGaurd Worker.
#
# The Worker is deployed as a swarm service that mounts the host's
# /var/run/docker.sock, so the runtime image only needs the static binary.

# ---- build stage ----
FROM golang:1.25-alpine AS build
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/worker ./cmd/worker

# ---- runtime stage ----
FROM alpine:3.20
# ca-certificates: for TLS against remote Docker daemons (tcp://host:2376)
# and for future outbound calls. tzdata keeps UTC timestamps sane.
RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.aliyun.com/g' /etc/apk/repositories && \
    apk add --no-cache ca-certificates tzdata
COPY --from=build /out/worker /usr/local/bin/worker
ENTRYPOINT ["/usr/local/bin/worker"]
