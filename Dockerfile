# HexClaw — 多阶段构建
#
# 阶段 1: 编译 Go 二进制
# 阶段 2: 最小运行时镜像

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

ARG GOPRIVATE=github.com/hexagon-codes/*
ENV GOPRIVATE=${GOPRIVATE}

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=netrc,target=/root/.netrc go mod download

COPY . .
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /hexclaw ./cmd/hexclaw

# ---

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /hexclaw /usr/local/bin/hexclaw

RUN mkdir -p /data/.hexclaw
ENV HOME=/data

EXPOSE 6060
ENTRYPOINT ["hexclaw"]
CMD ["serve", "--config", "/data/.hexclaw/hexclaw.yaml"]
