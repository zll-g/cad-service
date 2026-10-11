# syntax=docker/dockerfile:1
#
# cad-service 镜像：go-cad 解析服务
#
#   docker build -t cad-service:latest .
#
# 运行阶段是 FROM scratch（空镜像），整个构建只需要从 Docker Hub 拉取 golang:1.25-alpine，
# 不依赖 gcr.io / distroless。Go 模块默认经 goproxy.cn 下载，连不上时自动回退到 proxy.golang.org，
# 无法访问 Google 服务的网络不需要任何额外参数。
#
# 可选构建参数：
#   GOPROXY        Go 模块代理，默认 https://goproxy.cn|https://proxy.golang.org|direct
#                  （"|" 表示前一个出现任何错误、包括连不上时，自动换下一个；
#                    模块内容由 go.sum 校验，从哪个代理下载都不影响安全性）
#   ALPINE_MIRROR  安装字体用的 Alpine 软件源，例如 https://mirrors.aliyun.com
#                  （默认 dl-cdn.alpinelinux.org，台湾/香港一般可以直接访问）
#   WITH_CJK_FONT  是否内置中文字体，默认 true。只做文本提取（render=false）可设为 false，
#                  镜像小约 4.5MB，构建时也不需要访问 Alpine 软件源
#
# 多架构：
#   docker buildx build --platform linux/amd64,linux/arm64 -t <registry>/cad-service:0.1.0 --push .
#
# 带 shell 的调试版本（基于 alpine）：
#   docker build --target debug -t cad-service:debug .

ARG GO_IMAGE=golang:1.25-alpine

# ---------- 阶段 1：编译 ----------
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG GOPROXY=https://goproxy.cn|https://proxy.golang.org|direct
ARG TARGETOS
ARG TARGETARCH
ARG GOSUMDB=sum.golang.org
ENV GOPROXY=${GOPROXY} GOSUMDB=${GOSUMDB} CGO_ENABLED=0
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# selftest_attrib.dxf 通过 go:embed 编进二进制（属性坐标自检，见 cadfix.go）
COPY *.go selftest_attrib.dxf ./
# -tags timetzdata：把时区数据库编译进二进制（scratch 里没有 /usr/share/zoneinfo）
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -tags timetzdata -ldflags="-s -w" -o /out/cad-service .

# ---------- 阶段 2：组装根文件系统（字体 + /tmp） ----------
# 在构建机架构上执行（字体文件与 CPU 架构无关），复用上面已经拉取的 golang:alpine，
# 不需要额外拉取基础镜像。
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS rootfs
ARG ALPINE_MIRROR=
ARG WITH_CJK_FONT=true
# 为什么需要字体：go-cad 渲染 PNG 时，中文字形取自 /usr/share/fonts 下的系统字体，
# 没有字体时渲染图里的中文会全部丢失（文本提取不受影响）。
# Alpine 把字体装在 /usr/share/fonts/droid-nonlatin/，而 go-cad 只探测 Debian 风格路径，
# 所以必须复制到 /usr/share/fonts/truetype/droid/。
# Droid Sans Fallback 覆盖简体和繁体（已用常见工程用字和全角符号验证）。
RUN mkdir -p /rootfs/tmp /rootfs/usr/share/fonts/truetype/droid && chmod 1777 /rootfs/tmp && \
    if [ "$WITH_CJK_FONT" = "true" ]; then \
      if [ -n "$ALPINE_MIRROR" ]; then \
        sed -i -e "s#https://dl-cdn.alpinelinux.org#${ALPINE_MIRROR}#g" \
               -e "s#http://dl-cdn.alpinelinux.org#${ALPINE_MIRROR}#g" /etc/apk/repositories; \
      fi && \
      apk add --no-cache font-droid-nonlatin && \
      cp /usr/share/fonts/droid-nonlatin/DroidSansFallbackFull.ttf /rootfs/usr/share/fonts/truetype/droid/ && \
      chmod 644 /rootfs/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf; \
    fi

# ---------- 可选：带 shell 的调试版本 ----------
FROM alpine:3.22 AS debug
COPY --from=rootfs /rootfs/ /
COPY --from=build /out/cad-service /usr/local/bin/cad-service
ENV CAD_SERVICE_ADDR=0.0.0.0:8090
USER 65532:65532
EXPOSE 8090
ENTRYPOINT ["/usr/local/bin/cad-service"]

# ---------- 阶段 3：运行（默认构建的最后一个阶段） ----------
# scratch = 空镜像：不需要拉取、没有 shell、攻击面最小。
# 二进制完全静态（go-cad 零 cgo），时区数据已内嵌，字体和 /tmp 来自 rootfs。
FROM scratch
LABEL org.opencontainers.image.title="cad-service" \
      org.opencontainers.image.description="DWG/DXF parsing service powered by go-cad" \
      org.opencontainers.image.licenses="Apache-2.0"

COPY --from=rootfs /rootfs/ /
COPY --from=build /out/cad-service /usr/local/bin/cad-service

ENV CAD_SERVICE_ADDR=0.0.0.0:8090 \
    CAD_MAX_UPLOAD_MB=200 \
    TZ=Asia/Taipei
# scratch 里没有 /etc/passwd，用数字 UID 以非 root 身份运行
USER 65532:65532
EXPOSE 8090

# scratch 里没有 curl，使用二进制自带的健康检查子命令（exec 形式，不需要 shell）
HEALTHCHECK --interval=15s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/usr/local/bin/cad-service", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/cad-service"]
