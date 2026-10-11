// cad-service：把 go-cad 包装成 HTTP 微服务，供 Java 等非 Go 系统调用。
//
// 接口：
//
//	GET  /healthz                       健康检查
//	POST /v1/parse  (multipart: file)   解析 DWG/DXF，返回结构化结果（JSON，schemaVersion=2）：
//	                                    图纸（图号/图名/专业/比例/标题栏键值）、文本块（说明/表格/
//	                                    图例/标题栏）、块统计（设备数量 + 图例名称）、带属性的块参照、
//	                                    尺寸标注、图层、原始文字（含来源角色）、告警
//	     ?render=true                   同时返回每个图框的渲染图（base64），默认 false
//	     &format=png|svg                渲染格式，默认 png
//	     &width=N                       渲染宽度，默认 0（拆图自适应）
//
// 设计要点：
//   - 默认只做「解析 + 图框识别 + 文本归属 + 版面分析」，不渲染，速度快，适合批量入库；
//   - 需要多模态（把图纸图片交给视觉大模型生成描述）时再加 render=true；
//   - 解析过程 recover panic，单个坏文件不会拖垮服务。
//
// 容器相关（环境变量）：
//
//	CAD_SERVICE_ADDR     监听地址，默认 0.0.0.0:8090
//	CAD_MAX_UPLOAD_MB    单文件上限（MB），默认 200
//	CAD_MAX_CONCURRENCY  同时解析的文件数上限，默认 = CPU 核数；超出排队，
//	                     排队超过 CAD_QUEUE_TIMEOUT 秒（默认 120）返回 503
//	CAD_SHUTDOWN_GRACE   收到 SIGTERM 后等待进行中请求的秒数，默认 60
//	                     （docker stop 默认只等 10 秒，需配合 stop_grace_period 调大）
//	CAD_FONT_PATH        渲染用字体文件（.ttf/.otf/.ttc）。不设置时 go-cad 自动探测
//	                     /usr/share/fonts 下的 CJK 字体（镜像已内置 Droid Sans Fallback）。
//	                     注意：指定后 go-cad 不再使用后备字体链，缺字的字符会显示不出来
//	CAD_MAX_INSERTS      响应中 inserts[]（带属性的块参照明细）上限，默认 20000
//	CAD_MAX_DIMENSIONS   响应中 dimensions[] 上限，默认 20000
//	CAD_FIX_ATTRIB       auto|on|off，默认 auto：启动自检 go-cad 是否重复变换属性坐标，
//	                     存在问题时自动修正（见 cadfix.go）
//	GOMEMLIMIT           Go 运行时软内存上限，建议设为容器内存限制的 80%~90%
//
// 健康检查：`cad-service -healthcheck` 请求本机 /healthz，成功退出码 0；
// distroless 镜像里没有 curl/wget，Docker HEALTHCHECK 用它。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// go-cad 渲染时探测的系统字体路径（与 go-cad internal/render/render_text.go 保持一致）。
// 没有任何 CJK 字体时，渲染出的 PNG 会丢失全部中文，而且 go-cad 的清晰度自检
// （passRate）发现不了这种情况，所以启动时主动检查并打印警告。
var cjkFontCandidates = []string{
	"/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc",
	"/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf",
	"/usr/share/fonts/opentype/ipafont-gothic/ipagp.ttf",
	"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/truetype/wqy/wqy-microhei.ttc",
	"/usr/share/fonts/opentype/unifont/unifont.otf",
}

var fontPath string // CAD_FONT_PATH

func checkFonts() {
	if fontPath != "" {
		if _, err := os.Stat(fontPath); err != nil {
			log.Printf("WARNING: CAD_FONT_PATH=%s 不可读（%v），render=true 时中文可能无法显示", fontPath, err)
		} else {
			log.Printf("render font: %s (CAD_FONT_PATH)", fontPath)
		}
		return
	}
	for _, p := range cjkFontCandidates {
		if _, err := os.Stat(p); err == nil {
			log.Printf("render font: %s", p)
			return
		}
	}
	log.Printf("WARNING: 未找到 CJK 字体，render=true 渲染的 PNG 会丢失全部中文（文本提取不受影响）。" +
		"请使用内置字体的镜像，或挂载字体并设置 CAD_FONT_PATH")
}

var (
	maxUpload    int64         = 200 << 20 // 单文件上限
	sem          chan struct{}             // 并发闸门：大图纸解析/渲染吃内存，防止容器被 OOM Kill
	queueTimeout = 120 * time.Second
)

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return def
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "请求本机 /healthz 后退出（供 Docker HEALTHCHECK 使用）")
	flag.Parse()

	addr := os.Getenv("CAD_SERVICE_ADDR")
	if addr == "" {
		addr = "0.0.0.0:8090"
	}
	if *healthcheck {
		os.Exit(runHealthcheck(addr))
	}

	maxUpload = int64(envInt("CAD_MAX_UPLOAD_MB", 200)) << 20
	conc := envInt("CAD_MAX_CONCURRENCY", runtime.NumCPU())
	sem = make(chan struct{}, conc)
	queueTimeout = time.Duration(envInt("CAD_QUEUE_TIMEOUT", 120)) * time.Second
	shutdownGrace := time.Duration(envInt("CAD_SHUTDOWN_GRACE", 60)) * time.Second
	fontPath = os.Getenv("CAD_FONT_PATH")
	maxInserts = envInt("CAD_MAX_INSERTS", maxInserts)
	maxDimensions = envInt("CAD_MAX_DIMENSIONS", maxDimensions)
	checkFonts()
	initAttribFix()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/v1/parse", handleParse)
	srv := &http.Server{Addr: addr, Handler: mux, ReadTimeout: 5 * time.Minute, WriteTimeout: 10 * time.Minute}

	// 优雅退出：docker stop / k8s 滚动发布发送 SIGTERM 时，等正在解析的请求完成。
	// 注意 Shutdown 一调用 ListenAndServe 就会立刻返回，必须等 Shutdown 本身结束
	// 才能退出 main，否则进行中的请求会被直接掐断。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		log.Printf("shutting down, waiting for in-flight requests (max %s)...", shutdownGrace)
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("cad-service listening on %s (maxUpload=%dMB, concurrency=%d, GOMAXPROCS=%d)",
		addr, maxUpload>>20, conc, runtime.GOMAXPROCS(0))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
	log.Printf("bye")
}

func runHealthcheck(addr string) int {
	port := addr[strings.LastIndex(addr, ":")+1:]
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}

func handleParse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpErr(w, http.StatusMethodNotAllowed, "only POST")
		return
	}
	// 并发闸门：在读取上传内容之前排队（避免排队请求先把大文件读进内存），超时返回 503 让调用方重试
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-time.After(queueTimeout):
		httpErr(w, http.StatusServiceUnavailable, "server busy, retry later")
		return
	case <-r.Context().Done():
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	file, hdr, err := r.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file too large (limit %dMB, set CAD_MAX_UPLOAD_MB)", maxUpload>>20))
			return
		}
		httpErr(w, http.StatusBadRequest, "missing multipart field 'file': "+err.Error())
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		httpErr(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}

	q := r.URL.Query()
	withRender := q.Get("render") == "true"
	format := strings.ToLower(q.Get("format"))
	if format != "svg" {
		format = "png"
	}
	width, _ := strconv.Atoi(q.Get("width"))

	if kind := unsupportedFormat(data); kind != "" {
		log.Printf("rejected %s (%d bytes): %s", hdr.Filename, len(data), kind)
		httpErr(w, http.StatusUnsupportedMediaType, kind+" 不是 DWG/DXF，go-cad 无法解析；请在 CAD 里另存为 DWG/DXF，或走 DWF 专用解析通道")
		return
	}

	start := time.Now()
	resp, err := analyze(hdr.Filename, data, withRender, format, width)
	if err != nil {
		httpErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	resp.ElapsedMs = time.Since(start).Milliseconds()
	log.Printf("parsed %s (%d bytes) sheets=%d texts=%d blocks=%d tables=%d dims=%d warnings=%d render=%v in %dms [%s]",
		hdr.Filename, len(data), len(resp.Sheets), len(resp.Texts), len(resp.Blocks), resp.Stats.Tables,
		len(resp.Dimensions), len(resp.Warnings), withRender, resp.ElapsedMs, resp.timings)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// unsupportedFormat 按文件头识别常见的“看起来像 CAD 但不是 DWG/DXF”的格式（不信任扩展名）。
func unsupportedFormat(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("(DWF V")):
		return "DWF"
	case bytes.HasPrefix(data, []byte("PK\x03\x04")) && bytes.Contains(data[:min(len(data), 4096)], []byte("[Content_Types].xml")):
		return "DWFx/OPC"
	case bytes.HasPrefix(data, []byte("%PDF")):
		return "PDF"
	}
	return ""
}
