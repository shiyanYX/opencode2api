# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

ENV GOPROXY=https://goproxy.cn,direct

COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./
# go:embed 的静态资产（oc_shape_v2.go 嵌入 oc_prompt_v2.txt 与 oc_tools_v2.json；
# embed 在编译期读文件，缺了它们多架构构建必炸——2026-10-26 Docker 工作流
# 第七批 merge 即因此失败，且 CI 本机不复现：本地构建上下文含整个工作区）。
COPY oc_prompt_v2.txt oc_tools_v2.json ./
COPY static/ static/

RUN set -eux; \
    if [ "$TARGETARCH" = "arm" ]; then export GOARM="${TARGETVARIANT#v}"; fi; \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
      go build -trimpath \
      -ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" \
      -o /out/opencode2api .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates su-exec tzdata wget \
    && addgroup -S opencode2api \
    && adduser -S -G opencode2api -h /data opencode2api \
    && mkdir -p /app /data \
    && chown -R opencode2api:opencode2api /data

WORKDIR /data

COPY --from=build /out/opencode2api /usr/local/bin/opencode2api
COPY config.example.json /app/config.example.json
COPY docker/entrypoint.sh /entrypoint.sh

ENV OPENCODE2API_PORT=8000 \
    OPENCODE2API_CONFIG=/data/config.json \
    OPENCODE2API_PASSWORD=123456 \
    TZ=Asia/Shanghai

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:${OPENCODE2API_PORT}/health" >/dev/null || exit 1

ENTRYPOINT ["/entrypoint.sh"]
