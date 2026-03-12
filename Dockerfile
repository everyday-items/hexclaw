# HexClaw — 多阶段构建
#
# 阶段 1: 编译 Go 二进制
# 阶段 2: 最小运行时镜像

FROM golang:1.23-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /hexclaw ./cmd/hexclaw

# ---

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /hexclaw /usr/local/bin/hexclaw

RUN mkdir -p /data/.hexclaw
ENV HOME=/data

EXPOSE 6060
ENTRYPOINT ["hexclaw"]
CMD ["serve", "--config", "/data/.hexclaw/hexclaw.yaml"]
