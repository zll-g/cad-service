# Java RAG 知识库接入 go-cad 解析 CAD 图纸

go-cad 是 Go 库，Java 不能直接调用。推荐做法是把它包装成一个**独立的 HTTP 解析服务**，Java 侧只负责调用接口、切片和入库。

```
                ┌──────────────────────── Java RAG 应用 ────────────────────────┐
 .dwg/.dxf ──►  │ 文件路由 ──► CadParser ──► CadChunker ──► Embedding ──► 向量库 │
                └───────────────│───────────────────────────────────────────────┘
                                │ HTTP multipart
                       ┌────────▼────────┐
                       │  cad-service    │  Go + go-cad（单二进制，零 cgo，Docker 镜像约 10MB）
                       │  POST /v1/parse │  解析 → 图框识别 → 文本归属 → 版面分析（说明/表格/图例/标题栏）
                       └─────────────────┘  → 设备统计与属性 → 尺寸标注 → [可选] 拆图渲染
```

## 三种集成方式对比

| 方式 | 做法 | 优点 | 缺点 | 推荐度 |
|---|---|---|---|---|
| **A. HTTP 微服务**（本项目 `cad-service`） | Go 写一个薄服务，Java 调接口 | 进程隔离、可横向扩容；**可以只解析文本、不渲染，速度快** | 多部署一个服务 | ⭐⭐⭐ 生产首选 |
| **B. 调用 CLI**（`caddocling`） | Java `ProcessBuilder` 调官方命令行，读 `manifest.json` | 不用额外开发，复制一个二进制就能用 | **每次都会渲染拆图**，大图纸慢（实测一个测试 DWG 因清晰度自检反复放大重渲，用了 20 秒）；有进程启动开销 | ⭐⭐ 原型 / 小批量 |
| C. JNI / JNA / Panama | `go build -buildmode=c-shared` 编译成 .so 后在 JVM 内调用 | 没有网络开销 | 需要 cgo，抵消了 go-cad“零 cgo”的优势；崩溃会拖垮 JVM；跨平台编译麻烦 | ⭐ 不推荐 |

Java 侧用统一的 `CadParser` 接口封装了 A 和 B，两种实现可以随时互换。

## 目录

```
cad-service/            # Go 解析服务（方式 A）
├── main.go             # HTTP 入口：健康检查、并发闸门、优雅退出、非 DWG/DXF 拦截
├── analyze.go          # 增强分析主流程（schemaVersion=2 响应组装）
├── extract.go          # 遍历实体：块参照/属性/尺寸标注/图层，文字来源标注
├── layout.go           # 版面分析：文字聚行 → 文本块 → 说明/表格/图例分类、表格还原
├── titleblock.go       # 标题栏键值（设计/校对/审核/日期…）、比例、专业推断
├── cadfix.go           # go-cad 属性坐标问题的修正（启动自检，自动开关）
├── selftest_attrib.dxf # 上面自检用的最小 DXF（go:embed 进二进制）
├── reflectx.go         # 读取 go-cad 内部实体字段的反射工具
├── model.go            # 响应结构定义
├── analyze_test.go     # 回归测试（go test ./...）
├── go.mod / go.sum     # 锁定 go-cad 版本
├── Dockerfile          # 多阶段构建，scratch 运行（内置中文字体），支持多架构
├── .dockerignore
├── docker-compose.yml      # 一键启动 cad-service + Java 演示
├── .env.example            # 镜像仓库 / 国内镜像源配置
├── java-client/            # Java 端（JDK 11+，只依赖 Jackson）
│   ├── pom.xml             # shade 插件打 fat jar
│   ├── Dockerfile          # 编译 caddocling + Maven 打包 + JRE 运行
│   └── .dockerignore
│   └── src/main/java/com/example/cadrag/
│       ├── CadParser.java        # 解析器接口
│       ├── HttpCadParser.java    # 方式 A：调用 cad-service
│       ├── CliCadParser.java     # 方式 B：调用 caddocling
│       ├── CadParseResult.java   # 解析结果模型
│       ├── CadChunker.java       # ★ CAD 专用切片器
│       ├── CadChunk.java         # 切片（text + metadata）
│       └── Demo.java             # 命令行演示，输出 JSONL
└── samples/                # 测试样例与实际输出
    ├── make_sample_v2.py            # 生成“接近生产”的消防图：属性图框、带属性的设备块、图例、材料表、尺寸
    ├── fire_alarm_production_sample.dxf
    ├── parse_v2_fire_alarm.json     # 上面样例的 /v1/parse 完整响应
    ├── chunks_v2.jsonl              # v2 切片器的实际输出（含 3 个 go-cad 测试 DWG）
    ├── production_sheet1.png        # render=true 的拆图渲染结果
    ├── make_sample.py / fire_weak_current_sample.dxf   # 第一版简单样例
    └── chunks_http.jsonl / sheet1.png                  # 第一版输出（v1 切片器）
```

## 1. 部署 cad-service

```bash
cd cad-service
go build -o cad-service . && ./cad-service          # 默认监听 0.0.0.0:8090，可用 CAD_SERVICE_ADDR 修改
```

Docker 方式见下方[Docker 构建与部署](#docker-构建与部署)。

接口：

```bash
# 只取文本（入库用，快）
curl -F file=@图纸.dwg http://localhost:8090/v1/parse
# 同时返回每个图框的 PNG（base64），给多模态模型用
curl -F file=@图纸.dwg "http://localhost:8090/v1/parse?render=true&format=png"
```

非 DWG/DXF 文件（DWF、DWFx、PDF，按文件头判断，不看扩展名）直接返回 **415** 和中文说明，不会再报看不懂的“不支持的 DWG 版本”。

### 返回结构（schemaVersion = 2）

v1 的字段（`source / dwgVersion / hasFrames / sheets[].index,name,box,textCount / texts[] / elapsedMs`）**全部保留、含义不变**，老客户端只要忽略未知字段就能继续用。新增内容：

| 字段 | 内容 | 能回答的问题 |
|---|---|---|
| `file` | 文件名、格式、大小、**sha256**、DWG 版本、对应 AutoCAD 版本 | sha256 可直接当文档 ID：重复上传去重、图纸改版后删旧切片 |
| `sheets[].number / title` | 图号、图名（拆开的） | “XF-01 是什么图？” |
| `sheets[].titleBlock` | 标题栏键值：工程名称/图号/图名/设计/制图/校对/审核/审定/日期/比例/版次/设计单位… | “这张图谁设计的？哪天出的？” |
| `sheets[].discipline / scale` | 专业（由图号前缀和图名推断）、比例 | 按专业过滤检索 |
| `sheets[].blockCounts` | 本图设备/符号数量（**已排除图例里的示意符号和图框**），带图例中文名 | “地下一层有几个感烟探测器？” |
| `textBlocks[]` | 版面分析结果：`notes` 说明、`table` 表格（还原成表头 + 行）、`legend` 图例、`title_block` 标题栏、`text` 普通文字 | “材料表里报警总线用多少米？” |
| `blocks[]` | 块定义统计：类型（symbol/frame/…）、数量、各图数量、属性 TAG、图层 | 全套设备合计 |
| `inserts[]` | 带属性的块参照明细：块名、图例名称、坐标、所在图、属性值（编号、回路…） | “1-05 在哪个回路？” |
| `dimensions[]` | 尺寸标注：类型、实测值、显示文字（含“约6000(现场核实)”这类文字替代） | “轴距多少？” |
| `layers[]` | 图层：颜色、实体数、文字数、块参照数 | |
| `texts[].role` | 每条文字的来源/用途：`notes / table / legend / title_block / text / attribute / block_text / dimension / attdef` | 自定义切片时按需过滤 |
| `stats` | 实体数、实体类型分布、跳过的对象数、修正的属性数等 | 监控解析质量 |
| `warnings[]` | 质量提示：未识别到图框、尺寸值为计算所得、明细被截断… | 建议记录日志 |

返回示例（节选自 `samples/parse_v2_fire_alarm.json`，完整内容见文件）：

```json
{
  "schemaVersion": 2,
  "source": "fire_alarm_production_sample.dxf",
  "file": {"format": "dxf", "sha256": "f4e83e66…", "dwgVersion": "AC1024", "cadRelease": "AutoCAD 2010-2012"},
  "hasFrames": true,
  "sheets": [{
    "index": 1, "name": "XF-01-地下一层火灾自动报警平面图",
    "number": "XF-01", "title": "地下一层火灾自动报警平面图", "discipline": "消防", "scale": "1:100",
    "titleBlock": {"工程名称": "某某科技园1#楼", "设计": "张伟", "校对": "王芳", "审核": "李强", "日期": "2026.09", "…": "…"},
    "blockCounts": [{"block": "FA-SD", "label": "感烟探测器", "count": 12},
                    {"block": "FA-MCP", "label": "手动报警按钮", "count": 4}]
  }],
  "textBlocks": [
    {"id": 4, "sheet": 1, "type": "table", "title": "主要设备材料表",
     "table": {"header": ["序号","名称","型号规格","单位","数量","备注"],
               "rows": [["4","报警总线","WDZN-RYJS-2x1.5","m","860","穿SC20"]]}},
    {"id": 7, "sheet": 1, "type": "legend", "title": "图例",
     "lines": ["图例", "[FA-SD] | 感烟探测器 | JTY-GD-G3 | 吸顶安装"]}
  ],
  "inserts": [{"block": "FA-SD", "label": "感烟探测器", "kind": "symbol", "sheet": 1, "x": 6000, "y": 30000,
               "layer": "E-FA-设备", "attributes": {"回路": "L1", "编号": "1-01"}}],
  "dimensions": [{"type": "linear", "value": 6000, "text": "约6000(现场核实)", "computed": true, "sheet": 1}],
  "warnings": ["4 个尺寸标注的数值由定义点计算得到（DXF 中标注文字不在 texts 里，请使用 dimensions）"]
}
```

性能（纯文本模式，2 核）：样例图 6～20ms；合成的 9MB 大图（6 张图框、42000 条文字、6000 个带属性设备）约 0.8 秒，峰值内存约 240MB。日志会打印各阶段耗时，例如 `[parse=189ms sheets=63ms texts=119ms enrich=394ms]`，方便定位慢文件。

## Docker 构建与部署

### 镜像清单

| 镜像 | Dockerfile | 内容 | 大小（估算） |
|---|---|---|---|
| `cad-service` | `cad-service/Dockerfile` | `FROM scratch`：静态二进制（内嵌时区数据）+ 中文字体 + /tmp，无 shell、非 root 运行 | 约 14MB（二进制 9.3MB + 字体 4.5MB） |
| `cad-rag-client` | `java-client/Dockerfile` | fat jar + JRE 17 + 内置 `caddocling`（CLI 模式用） | 约 280MB（主要是 JRE） |

两个 Dockerfile 都是多阶段构建，并支持 BuildKit 缓存挂载（Go 模块和 Maven 仓库在多次构建之间复用）。需要 **Docker 23+**（默认启用 BuildKit）。

### 一键启动（docker compose）

```bash
cp .env.example .env                                  # 国内网络：打开里面的镜像源配置
docker compose up -d --build cad-service              # 启动解析服务
docker compose ps                                     # 状态应为 healthy
curl -F file=@samples/fire_weak_current_sample.dxf http://localhost:8090/v1/parse

docker compose --profile demo up --build cad-demo     # Java 演示（HTTP 模式），输出到 ./output/
docker compose --profile demo run --rm cad-demo-cli   # Java 演示（CLI 模式，不经过 cad-service）
```

### 单独构建

```bash
# cad-service
docker build -t cad-service:0.1.0 ./cad-service

# Alpine 软件源慢或连不上时（Go 模块默认已经走 goproxy.cn，不用再指定）
docker build -t cad-service:0.1.0 \
  --build-arg ALPINE_MIRROR=https://mirrors.aliyun.com \
  ./cad-service

# 只做文本提取、不需要渲染图片：不装字体，镜像更小，构建时也不需要访问 Alpine 软件源
docker build -t cad-service:0.1.0-notext --build-arg WITH_CJK_FONT=false ./cad-service

# 需要进容器排查问题：构建带 shell 的 debug 版本（基于 alpine）
docker build --target debug -t cad-service:debug ./cad-service

# 多架构（x86 + ARM，例如鲲鹏/Graviton 服务器、Apple 芯片），直接推送到仓库
docker buildx build --platform linux/amd64,linux/arm64 \
  -t registry.example.com/rag/cad-service:0.1.0 --push ./cad-service

# Java 客户端（Maven Central 慢时加镜像源；只用 HTTP 模式可以不带 caddocling）
docker build -t cad-rag-client:0.1.0 \
  --build-arg MAVEN_MIRROR=https://maven.aliyun.com/repository/public \
  --build-arg WITH_CADDOCLING=true \
  ./java-client
```

### 所有构建参数

| 参数 | 镜像 | 默认值 | 说明 |
|---|---|---|---|
| `GOPROXY` | 两个都有 | `https://goproxy.cn\|https://proxy.golang.org\|direct` | `\|` 表示前一个代理出现任何错误（包括连不上）就自动换下一个，访问不了 Google 的网络也不需要改。模块内容由 `go.sum` 和校验和数据库验证，换哪个代理都不影响安全性 |
| `GO_IMAGE` | 两个都有 | `golang:1.25-alpine` | 构建用 Go 镜像（go-cad 要求 Go ≥ 1.25） |
| `ALPINE_MIRROR` | 两个都有 | 空（使用 dl-cdn.alpinelinux.org） | 安装字体用的 Alpine 软件源，例如 `https://mirrors.aliyun.com` |
| `WITH_CJK_FONT` | cad-service | `true` | 是否内置中文字体；`false` 时 render=true 渲染的图片会丢失中文 |
| `RUNTIME_IMAGE` | client | `eclipse-temurin:17-jre` | |
| `MAVEN_IMAGE` | client | `maven:3.9-eclipse-temurin-17` | |
| `MAVEN_MIRROR` | client | 空 | 如 `https://maven.aliyun.com/repository/public` |
| `GO_CAD_VERSION` | client | 与 `cad-service/go.mod` 相同 | 保证 CLI 模式与 HTTP 模式解析结果一致 |
| `WITH_CADDOCLING` | client | `true` | `false` 时不编译 caddocling，构建更快 |

### cad-service 运行时环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `CAD_SERVICE_ADDR` | `0.0.0.0:8090` | 监听地址 |
| `CAD_MAX_UPLOAD_MB` | `200` | 单文件上限，超过返回 **413** |
| `CAD_MAX_CONCURRENCY` | CPU 核数 | 同时解析的文件数，超出的请求排队 |
| `CAD_QUEUE_TIMEOUT` | `120` | 排队超过这个秒数返回 **503**，调用方应重试 |
| `CAD_SHUTDOWN_GRACE` | `60` | 收到 SIGTERM 后等待进行中请求的秒数 |
| `CAD_FONT_PATH` | 不设 | 自定义渲染字体（挂载进容器的 .ttf/.otf/.ttc）。注意：指定后 go-cad 不再使用后备字体，缺字的字符显示不出来 |
| `CAD_MAX_INSERTS` | `20000` | `inserts[]` 明细上限，超出截断并在 `warnings` 里提示（`blocks[]` 的统计不受影响） |
| `CAD_MAX_DIMENSIONS` | `20000` | `dimensions[]` 明细上限 |
| `CAD_FIX_ATTRIB` | `auto` | 属性坐标修正（见“注意事项”）。`auto` 启动时自检，当前 go-cad 版本有问题才开启；`on` / `off` 强制 |
| `GOMEMLIMIT` | 不设 | 建议设为容器内存上限的 90%，例如 `1800MiB` |
| `TZ` | `Asia/Taipei` | 日志时区（时区数据已编译进二进制，scratch 镜像里也能用） |

### 生产部署要点（都已在沙箱里实测）

- **网络要求**：Docker Hub（基础镜像）、goproxy.cn 或 proxy.golang.org 二者之一（Go 模块）、dl-cdn.alpinelinux.org（字体，可以换镜像站）、Maven Central（Java 镜像，可以换镜像站）。**不需要访问 gcr.io 和其他 Google 服务。**
- **只需要访问 Docker Hub**：cad-service 运行阶段是 `FROM scratch`，构建阶段只用到 `golang:1.25-alpine`；Java 镜像用的是 `golang`、`maven`、`eclipse-temurin`，全部来自 Docker Hub，不依赖 gcr.io。
- **内存是首要瓶颈**：带 `render=true` 渲染大图纸时，画布可达数亿像素，单个请求就可能占用上 GB 内存。务必同时设置：容器内存上限、`CAD_MAX_CONCURRENCY`（按“内存上限 ÷ 单请求峰值”估算）、`GOMEMLIMIT`。只做文本入库（`render=false`）时内存占用很小。
- **并发排队**：并发设为 1 时，第二个请求排队超时后返回 `503 server busy`，第一个请求正常完成。Java 侧遇到 503 应该退避重试。
- **优雅退出**：收到 SIGTERM 后不再接收新连接，进行中的请求处理完（实测返回 200）后进程才退出。**`stop_grace_period`（K8s 里是 `terminationGracePeriodSeconds`）必须大于 `CAD_SHUTDOWN_GRACE`**，否则 Docker 默认 10 秒后直接 SIGKILL。
- **只读根文件系统**：compose 里开启了 `read_only`，但必须挂载可写的 `/tmp`（tmpfs）。Go 解析 multipart 时，超过 32MB 的上传会暂存到 /tmp，没有它大文件上传会失败。
- **健康检查**：scratch 镜像里没有 curl，`HEALTHCHECK` 使用二进制自带的 `cad-service -healthcheck` 子命令。
- **Java 镜像必须设置 `LANG=C.UTF-8`**：已写进 Dockerfile。CLI 模式产生的文件名包含中文图名。
- **版本锁定**：go-cad 还没有发布正式版本，目前锁定在一个 commit 伪版本（`v0.0.0-20261001090957-ddd91eea9618`）。升级时同时修改 `cad-service/go.mod` 和 `java-client/Dockerfile` 里的 `GO_CAD_VERSION`，并用自己的图纸做回归测试。

### 中文字体（重要）

go-cad 的 README 说“内置 CJK 字形引擎、不依赖系统字体”，**这和代码不符**：渲染 PNG 时，中文字形是从系统字体读取的（按顺序探测 `/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf`、`/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc` 等路径），找不到时退回到只支持 ASCII 的点阵字体。

- **只影响 `render=true` 的图片**。文本提取（RAG 入库的主流程）完全不受影响。
- 没有字体时，**中文会整行消失，只剩数字和英文**；go-cad 的清晰度自检（`passRate`）仍然显示 1.00，**不会报任何警告**。对比图见 `samples/font_compare_v2.png`。
- 本项目的两个镜像都内置了 **Droid Sans Fallback**（约 4.5MB）。它对繁体工程用字（123/123）、简体工程用字（103/103）、全角和工程符号（58/58）的覆盖率都是 100%。
- 注意：Alpine 的 `font-droid-nonlatin` 包把字体装在 `/usr/share/fonts/droid-nonlatin/`，**不在 go-cad 的探测路径上**，Dockerfile 会把它复制到 `/usr/share/fonts/truetype/droid/`。如果你自己写 Dockerfile，`apk add` 之后也要做这一步；Debian/Ubuntu 的 `fonts-droid-fallback` 包路径本身就是对的。
- cad-service 启动时会检查字体，找不到就在日志里打印 `WARNING: 未找到 CJK 字体`。
- 想换字体（例如公司规定的字体）：挂载字体文件并设置 `CAD_FONT_PATH`，或者直接挂载到 `/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf`（这种方式保留后备字体链）。请注意字体授权，Windows 自带的微软雅黑、宋体等不能随意放进镜像分发。

### 构建排错

| 报错 | 原因 | 解决 |
|---|---|---|
| `failed to resolve source metadata for gcr.io/distroless/...`、`dialing gcr.io:443 ... connection attempt failed` | 用的是**旧版 Dockerfile**（运行阶段依赖 gcr.io），而当前网络连不上 gcr.io | 换成新版 Dockerfile。新版运行阶段是 `FROM scratch`，**只需要访问 Docker Hub** |
| `golang:1.25-alpine` / `maven` / `eclipse-temurin` 拉不下来 | 连不上 Docker Hub | Docker Desktop → Settings → Resources → Proxies 配置代理；或在 Docker Engine 配置里加 `registry-mirrors`；或用 `--build-arg GO_IMAGE=<私有仓库>/golang:1.25-alpine` 指定内部镜像 |
| `apk add font-droid-nonlatin` 超时 | 连不上 Alpine 软件源 | `--build-arg ALPINE_MIRROR=https://mirrors.aliyun.com`（或其他镜像站）；不需要渲染图片的话可以用 `WITH_CJK_FONT=false` 直接跳过 |
| `go mod download` 报 `proxy.golang.org ... connection refused` | 用的是旧版 Dockerfile（默认只走 proxy.golang.org），而当前网络访问不了 Google 服务 | 换成新版 Dockerfile（默认先走 goproxy.cn，失败再回退）；或者临时加 `--build-arg GOPROXY=https://goproxy.cn,direct` |
| `go mod download` 报 `verifying ... sum.golang.org` 相关错误 | 校验和数据库连不上 | goproxy.cn 会代理校验和数据库，用默认 GOPROXY 即可；仍有问题时加 `--build-arg GOSUMDB=sum.golang.google.cn` |
| `the --mount option requires BuildKit` | Docker 太旧或关闭了 BuildKit | 升级到 Docker 23+，或设置 `DOCKER_BUILDKIT=1` |
| Docker Desktop 提示 `no HTTPS proxy` | Docker Desktop 没有继承系统代理 | 在 Docker Desktop 的 Proxies 里手动填写 HTTP/HTTPS 代理，或者启用“使用系统代理” |

### 接入你自己的 Java 应用镜像

- **HTTP 模式（推荐）**：你的 Spring Boot 镜像**不需要任何改动**，只需要配置 `cad-service` 的地址（在 compose 或 K8s 里用服务名，例如 `http://cad-service:8090`）。
- **CLI 模式**：在你的 Dockerfile 里加一个阶段编译 caddocling 并复制过来：

```dockerfile
FROM golang:1.25-alpine AS caddocling
ENV CGO_ENABLED=0
RUN go install -trimpath -ldflags="-s -w" \
    github.com/unitedrhino/go-cad/cmd/caddocling@v0.0.0-20261001090957-ddd91eea9618

FROM eclipse-temurin:17-jre
ENV LANG=C.UTF-8 LC_ALL=C.UTF-8
COPY --from=caddocling /go/bin/caddocling /usr/local/bin/caddocling
# caddocling 会渲染图片，需要中文字体；Ubuntu/Debian 的这个包路径正好是 go-cad 的探测路径
RUN apt-get update && apt-get install -y --no-install-recommends fonts-droid-fallback \
    && rm -rf /var/lib/apt/lists/*
COPY target/your-app.jar /app/app.jar
ENTRYPOINT ["java", "-jar", "/app/app.jar"]
```

### Kubernetes 片段（参考）

```yaml
containers:
  - name: cad-service
    image: registry.example.com/rag/cad-service:0.1.0
    ports: [{ containerPort: 8090 }]
    env:
      - { name: CAD_MAX_CONCURRENCY, value: "4" }
      - { name: GOMEMLIMIT, value: "1800MiB" }
    resources:
      requests: { cpu: "1", memory: 1Gi }
      limits:   { cpu: "4", memory: 2Gi }
    readinessProbe: { httpGet: { path: /healthz, port: 8090 }, periodSeconds: 10 }
    livenessProbe:  { httpGet: { path: /healthz, port: 8090 }, periodSeconds: 15 }
    securityContext: { runAsNonRoot: true, readOnlyRootFilesystem: true, allowPrivilegeEscalation: false }
    volumeMounts: [{ name: tmp, mountPath: /tmp }]
terminationGracePeriodSeconds: 70
volumes:
  - { name: tmp, emptyDir: { medium: Memory, sizeLimit: 512Mi } }
```

## 2. Java 侧使用

```java
CadParser parser = new HttpCadParser("http://cad-service:8090");
// CadParser parser = new CliCadParser("/opt/bin/caddocling", Paths.get("/tmp/cad-work"), 300);

CadParseResult result = parser.parse(Paths.get("图纸.dwg"), false);
List<CadChunk> chunks = new CadChunker().maxChars(800).overlapLines(2).chunk(result);
```

### 接入 Spring AI

```java
@Component
public class CadIngestService {
    private final CadParser parser = new HttpCadParser("http://cad-service:8090");
    private final CadChunker chunker = new CadChunker();
    private final VectorStore vectorStore;

    public CadIngestService(VectorStore vectorStore) { this.vectorStore = vectorStore; }

    public void ingest(Path file) throws Exception {
        List<Document> docs = chunker.chunk(parser.parse(file, false)).stream()
                .map(c -> new Document(c.text, c.metadata))
                .collect(Collectors.toList());
        vectorStore.add(docs);   // 已经切好片，不要再过 TokenTextSplitter
    }
}

// 检索时按元数据过滤，例如只查消防专业、只查材料表：
vectorStore.similaritySearch(SearchRequest.builder()
        .query("报警总线型号和用量").topK(5)
        .filterExpression("docType == 'cad' && discipline == '消防' && chunkType == 'table'").build());

// 图纸改版重新入库：先按 source 删掉旧切片（或者用 chunkId 做 upsert）
vectorStore.delete(new FilterExpressionBuilder().eq("source", "某某科技园-消防.dwg").build());
```

### 接入 LangChain4j

```java
List<TextSegment> segments = chunker.chunk(parser.parse(file, false)).stream()
        .map(c -> TextSegment.from(c.text, Metadata.from(c.metadata)))
        .collect(Collectors.toList());
List<Embedding> embeddings = embeddingModel.embedAll(segments).content();
embeddingStore.addAll(embeddings, segments);
```

### 和现有文档解析流程合并

如果你已经用 Tika / Docling / Unstructured 处理 PDF、Word，只需要在文件分发处加一个分支：

```java
if (CadParser.isCadFile(filename)) {
    chunks = cadChunker.chunk(cadParser.parse(path, false));
} else {
    chunks = existingPipeline.process(path);
}
```

## 3. 为什么 CAD 要单独写切片器

CAD 文本和普通文档完全不同：没有段落顺序，文本散落在二维坐标上，还夹着大量尺寸数字和重复的块标注。把所有文本直接拼起来做 embedding，检索效果会很差。而且图纸里**最有用的信息很多根本不在“文字”里**：设备数量来自块参照，设备编号和回路来自块属性，设计人员和日期常常是图框块的属性。

服务端（schemaVersion 2）先把这些结构解析出来，`CadChunker` 再**按内容类型切片**，每种切片回答一类问题：

| chunkType | 粒度 | 内容 | 典型问题 |
|---|---|---|---|
| `drawing_index` | 每个文件 1 个 | 图纸目录（图号、图名、专业、比例）+ 全套设备合计 | “这套图有哪些图纸？一共多少个感烟探测器？” |
| `sheet_summary` | 每张图 1 个 | 标题栏全部字段 + 本图设备数量 + 本图包含哪些说明/表格 + 尺寸标注 + 图层 | “XF-01 谁设计、谁审核？比例多少？” |
| `notes` | 每段说明 | 设计说明、系统说明等编号条目 | “探测器安装间距有什么要求？” |
| `table` | 每张表（长表分段，**每段重复表头**） | 材料表、设备表，转成 Markdown 表格 | “报警总线用什么型号？多少米？” |
| `legend` | 每个图例 | 符号块 → 名称、型号、安装方式 | “手动报警按钮装多高？” |
| `equipment` | 每张图每种设备 | 带属性的设备明细（编号、回路…），编号类属性排在前面 | “1-05 在哪个回路？L2 回路有哪些设备？” |
| `text` | 每张图 | 其余文字（房间名、系统图标注…），阅读顺序 + 去重计数，过滤纯尺寸数字 | “消防控制室在哪张图上？” |

设计上的几个取舍：

- **标题栏不单独成片**。标题栏是十几个两三个字的碎片，单独入库只会制造噪声，所以合并进 `sheet_summary`，同时写进每个切片的元数据。
- **每个切片都带完整上下文头**：`【CAD图纸】文件：xx；工程：某某科技园1#楼；图纸：XF-01 地下一层火灾自动报警平面图（消防，1:100）；内容：主要设备材料表`。切片单独被检索到时也知道出处，大模型可以准确引用。
- **元数据只用字符串和数字**，各家向量库都能过滤：`docId`(sha256)、`source`、`project`、`sheetIndex`、`sheetNumber`、`sheetTitle`、`discipline`、`scale`、`designer`、`checker`、`reviewer`、`date`、`chunkType`、`title`、`block`/`blockLabel`（equipment 切片）、`tableColumns`/`rowCount`（table 切片）、`chunkId`（稳定 ID，可用于 upsert）。
- **自动回退**：CLI 模式或老版本服务（没有 `schemaVersion`）时，按 v1 逻辑切片（按图框分组、阅读顺序、去重计数），`chunkType = sheet_text`。

实际输出（`samples/chunks_v2.jsonl`，样例图共 13 个切片）：

```
【CAD图纸】文件：fire_alarm_production_sample.dxf；工程：某某科技园1#楼；图纸：XF-01 地下一层火灾自动报警平面图（消防，1:100）
图纸信息：XF-01 地下一层火灾自动报警平面图
标题栏：工程名称：某某科技园1#楼；图号：XF-01；图名：地下一层火灾自动报警平面图；专业：消防；比例：1:100；日期：2026.09；设计：张伟；校对：王芳；审核：李强
本图设备/符号数量：感烟探测器(FA-SD) 12 个；手动报警按钮(FA-MCP) 4 个；声光警报器(FA-ALARM) 3 个
本图包含：设计说明、主要设备材料表、图例
尺寸标注 4 个：7200(×2)、8400、约6000(现场核实)
```

```
【CAD图纸】文件：fire_alarm_production_sample.dxf；工程：某某科技园1#楼；图纸：XF-01 地下一层火灾自动报警平面图（消防，1:100）；内容：主要设备材料表
表格：主要设备材料表
| 序号 | 名称 | 型号规格 | 单位 | 数量 | 备注 |
| --- | --- | --- | --- | --- | --- |
| 1 | 感烟探测器 | JTY-GD-G3 | 只 | 12 | 吸顶 |
| 4 | 报警总线 | WDZN-RYJS-2x1.5 | m | 860 | 穿SC20 |
```

```
【CAD图纸】文件：fire_alarm_production_sample.dxf；工程：某某科技园1#楼；图纸：XF-01 地下一层火灾自动报警平面图（消防，1:100）；内容：感烟探测器（FA-SD）
设备明细：感烟探测器（FA-SD），共 12 个
- 编号=1-01，回路=L1
- 编号=1-07，回路=L2
…
```

可以调整的参数：`maxChars`（默认 800）、`overlapLines`（默认 2，只作用于说明和普通文字）、`dropDimensionTexts`、`maxDimensionsInSummary`（默认 30）。

## 4. 进阶：多模态增强（可选）

图纸的大部分信息在图形里，不在文字里，比如平面布局和管线走向。可以这样补充：

1. 调用 `?render=true` 拿到每个图框的 PNG（go-cad 会自动保证文字清晰；中文字形依赖系统字体，本项目的镜像已内置，见“中文字体”一节）；
2. 把图片交给视觉大模型（Qwen-VL、GPT-4o 等），让它生成“这张图画了什么”的描述；
3. 把描述作为额外的切片入库，元数据和文本切片保持一致（同一个 `sheetIndex`）；
4. 检索命中后，可以把原图返回给前端展示，方便人工核对。

## 5. 注意事项与限制（实测发现）

- **图框识别有前提**：go-cad 只把**块参照（INSERT）**形式、短边 **≥ 500 个图形单位**、宽高比接近标准图幅的矩形块识别为图框。按设计院常规画法（图框是块、模型空间 1:100 放大）都没问题；如果图框是散线画的，或者 1:1 小尺寸，会退化成“整图”一个单元（`hasFrames=false`）。这种情况切片器仍能正常工作，只是不会按图分组。
- **图名提取有前提**（`sheets[].name`，go-cad 原生字段；v2 新增的 `number/title/titleBlock` 由服务端从标题栏键值提取，更可靠，优先用它们）：要求标题栏在图框**右下角**（宽 1/4 × 高 1/6 的范围内），图号符合 `XF-01`、`RD(XF)-05` 这类格式，图名在图号上方且右对齐。不满足时会退而选用标题栏里字号最大的文字（我第一次造的样例就取成了“设计单位”）。建议用你们自己的图纸抽样检查 `sheets[].name`，不准的话可以在 Java 侧按你们的标题栏模板自行提取。
- **CLI 模式与中文文件名**：`caddocling` 输出的拆图文件名包含中文图名。JVM 在非 UTF-8 环境（例如 Docker 默认的 POSIX locale）下无法用 `Path` 表示这些文件名，`CliCadParser` 已经做了兼容。即便如此，仍建议部署时设置 `LANG=C.UTF-8`。
- **CLI 模式速度**：每次都会渲染拆图，有时还会多次放大重渲以保证文字清晰，大图纸可能要几十秒。批量入库请用 HTTP 服务的纯文本模式（实测样例 100～600ms）。
- **go-cad 本身**：项目很新，三维实体（ACIS）不解析，R2.x～R8 老格式不支持。上线前请用自己的真实图纸做一轮回归测试。
- **大文件**：服务端单个文件上限 200MB（`CAD_MAX_UPLOAD_MB`）；建议 Java 侧对 CAD 解析单独设置线程池和超时。
- **go-cad 属性坐标问题（已在服务端修正）**：当前 go-cad 版本会把块属性（ATTRIB）的坐标再做一次块变换，可属性本来就是世界坐标。结果是图框放大 100 倍时，标题栏属性和设备编号会“飞”到图框外，渲染时也画错位置。`cadfix.go` 在启动时用内置的小 DXF 自检：确认有问题才修正（日志 `attrib position fix: ON`），go-cad 修复后会自动关闭，无需改代码。`stats.attribFixed` 是修正的数量。
- **属性默认值（ATTDEF）**：块定义里的属性默认值（如“编号”“回路”占位文字）会被 go-cad 当成文字输出，服务端把它们标为 `role=attdef`，不参与版面分析和切片。R13/R2000 文件里属性重复输出的问题也已去重。
- **超大坐标**：go-cad 会丢弃 |x| 或 |y| ≥ 1e7 的文字（例如直接画在测绘坐标系里的总图），这些文字不会出现在结果里。遇到时可以在 CAD 里把图移到原点附近再导出。
- **DXF 的尺寸标注**：很多 DXF 不保存实测值和标注块，服务端改用定义点计算（`computed=true`，并在 `warnings` 里提示）。用了“文字替代”的标注以图上文字为准。
- **标题栏 / 图例 / 表格是启发式识别**：按标签同义词（设计/设 计/DESIGNED BY…）、对齐关系和表头关键字判断。设计院自己的模板差异很大，上线前请抽样检查 `sheets[].titleBlock` 和 `textBlocks`。`analyze_test.go` 是现成的回归测试框架，可以把你们的典型图纸加进去。
- **DWF**：go-cad 不支持，服务端会直接返回 415。可以让设计方另存为 DWG/DXF，或者另外接一个 DWF 解析通道（上一轮讨论过的方案）。
