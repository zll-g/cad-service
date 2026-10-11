# syntax=docker/dockerfile:1
#
# Java 客户端镜像（演示 / 模板）：
#   - 内含 fat jar（HTTP 模式：调用 cad-service）
#   - 内含 caddocling 二进制（CLI 模式：不依赖 cad-service，单容器也能解析）
#
#   docker build -t cad-rag-client:latest .
#
# Go 模块默认经 goproxy.cn 下载，无法访问 Google 服务的网络不需要额外参数。
# Maven Central 下载慢或连不上时：
#   docker build \
#     --build-arg MAVEN_MIRROR=https://maven.aliyun.com/repository/public \
#     -t cad-rag-client:latest .
#
# 只用 HTTP 模式、不需要 caddocling（也就不需要字体）：
#   docker build --build-arg WITH_CADDOCLING=false -t cad-rag-client:latest .
#
# 本镜像只从 Docker Hub 拉取镜像（golang / maven / eclipse-temurin），不依赖 gcr.io。

ARG GO_IMAGE=golang:1.25-alpine
ARG MAVEN_IMAGE=maven:3.9-eclipse-temurin-17
ARG RUNTIME_IMAGE=eclipse-temurin:17-jre

# ---------- 阶段 1：编译 caddocling（CLI 模式用） ----------
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS caddocling
# 默认经 goproxy.cn 下载（同时代理校验和数据库），连不上时自动回退到 proxy.golang.org
ARG GOPROXY=https://goproxy.cn|https://proxy.golang.org|direct
# 与 cad-service/go.mod 中锁定的版本保持一致，保证两种模式解析结果相同
ARG GO_CAD_VERSION=v0.0.0-20261001090957-ddd91eea9618
ARG WITH_CADDOCLING=true
ARG ALPINE_MIRROR=
ARG TARGETOS
ARG TARGETARCH
# 注意：不能设置 GOBIN —— 交叉编译时 go install 遇到 GOBIN 会直接报错。
# 本机架构产物在 $GOPATH/bin/，交叉编译产物在 $GOPATH/bin/<os>_<arch>/，用 find 统一取出
ARG GOSUMDB=sum.golang.org
ENV GOPROXY=${GOPROXY} GOSUMDB=${GOSUMDB} CGO_ENABLED=0
RUN mkdir -p /out && if [ "$WITH_CADDOCLING" = "true" ]; then \
      GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
      go install -trimpath -ldflags="-s -w" \
        github.com/unitedrhino/go-cad/cmd/caddocling@${GO_CAD_VERSION} && \
      find "$(go env GOPATH)/bin" -name caddocling -type f -exec cp {} /out/caddocling \; && \
      test -x /out/caddocling; \
    else \
      printf '#!/bin/sh\necho "caddocling not included (WITH_CADDOCLING=false)" >&2; exit 1\n' > /out/caddocling; \
    fi && chmod +x /out/caddocling

# caddocling 每次都会渲染拆图，需要 CJK 字体，否则图片里的中文会全部丢失。
# Alpine 把字体装在 droid-nonlatin/ 目录，而 go-cad 只探测 Debian 风格路径，所以复制到 truetype/droid/
RUN mkdir -p /out/fonts && if [ "$WITH_CADDOCLING" = "true" ]; then \
      if [ -n "$ALPINE_MIRROR" ]; then \
        sed -i -e "s#https://dl-cdn.alpinelinux.org#${ALPINE_MIRROR}#g" \
               -e "s#http://dl-cdn.alpinelinux.org#${ALPINE_MIRROR}#g" /etc/apk/repositories; \
      fi && \
      apk add --no-cache font-droid-nonlatin && \
      cp /usr/share/fonts/droid-nonlatin/DroidSansFallbackFull.ttf /out/fonts/; \
    fi

# ---------- 阶段 2：Maven 打包 ----------
FROM ${MAVEN_IMAGE} AS build
ARG MAVEN_MIRROR=
WORKDIR /src
# 可选：配置 Maven 镜像源
RUN if [ -n "$MAVEN_MIRROR" ]; then mkdir -p /root/.m2 && printf '%s\n' \
      '<settings><mirrors><mirror><id>mirror</id><mirrorOf>*</mirrorOf>' \
      "<url>${MAVEN_MIRROR}</url></mirror></mirrors></settings>" > /root/.m2/settings.xml; fi
# 先只复制 pom 解析依赖，源码变动时不必重新下载
COPY pom.xml .
RUN --mount=type=cache,target=/root/.m2/repository mvn -B -q dependency:go-offline
COPY src ./src
RUN --mount=type=cache,target=/root/.m2/repository mvn -B -q -DskipTests package

# ---------- 阶段 3：运行 ----------
FROM ${RUNTIME_IMAGE}
# LANG=C.UTF-8 很重要：caddocling 输出的拆图文件名包含中文图名，
# JVM 在 POSIX locale 下会把这些路径当作非法字符
ENV LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    TZ=Asia/Taipei \
    JAVA_TOOL_OPTIONS="-XX:MaxRAMPercentage=75 -Dfile.encoding=UTF-8"

COPY --from=caddocling /out/caddocling /usr/local/bin/caddocling
COPY --from=caddocling /out/fonts/ /usr/share/fonts/truetype/droid/
WORKDIR /app
COPY --from=build /src/target/cad-rag-client.jar /app/app.jar

# 非 root 运行；/app 需要可写（CLI 模式在 /app/cad-work 下生成临时文件）
RUN groupadd -r -g 10001 app && useradd -r -u 10001 -g app -d /app app \
    && mkdir -p /app/cad-work /out && chown -R app:app /app /out
USER app

ENTRYPOINT ["java", "-jar", "/app/app.jar"]
# 默认参数：HTTP 模式，结果写到 /out/chunks.jsonl；docker run 时可以覆盖
CMD ["http", "http://cad-service:8090", "/out/chunks.jsonl", "/data/fire_weak_current_sample.dxf"]
