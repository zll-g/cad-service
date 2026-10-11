package main

// 解析主流程：go-cad 解析 → 属性坐标修正 → 图框识别 → 文字提取 → 结构化增强。

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	cad "github.com/unitedrhino/go-cad"
)

var (
	maxInserts    = 20000 // CAD_MAX_INSERTS：inserts[] 明细上限
	maxDimensions = 20000 // CAD_MAX_DIMENSIONS：dimensions[] 明细上限
)

var cadReleases = map[string]string{
	"AC1004": "AutoCAD R9", "AC1006": "AutoCAD R10", "AC1009": "AutoCAD R11/R12",
	"AC1012": "AutoCAD R13", "AC1014": "AutoCAD R14", "AC1015": "AutoCAD 2000-2002",
	"AC1018": "AutoCAD 2004-2006", "AC1021": "AutoCAD 2007-2009", "AC1024": "AutoCAD 2010-2012",
	"AC1027": "AutoCAD 2013-2017", "AC1032": "AutoCAD 2018-2025",
}

func analyze(filename string, data []byte, withRender bool, format string, width int) (resp *parseResp, err error) {
	// go-cad 自称零 panic，但服务端仍兜底，避免单个异常文件导致进程退出
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("parse panic: %v", p)
		}
	}()

	ext := strings.ToLower(filepath.Ext(filename))
	stage := newStageTimer()
	var doc *cad.Document
	fileFormat := "dwg"
	switch ext {
	case ".dxf", ".dxfb":
		fileFormat = "dxf"
		doc, err = cad.ParseDXF(data)
	default:
		doc, err = cad.Parse(data)
	}
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filename, err)
	}

	stage.mark("parse")
	attribFixed := fixAttribPositions(doc) // 必须在 DetectSheets/SheetTexts/Render 之前
	sheets := cad.DetectSheets(doc)
	stage.mark("sheets")
	hasFrames := !(len(sheets) == 1 && sheets[0].Name == "整图")
	texts := dedupTexts(cad.SheetTexts(doc))
	stage.mark("texts")
	ex := extract(doc, maxInserts)

	sum := sha256.Sum256(data)
	resp = &parseResp{
		SchemaVersion: 2,
		Source:        filename,
		DwgVersion:    doc.Version(),
		HasFrames:     hasFrames,
		File: fileDTO{Name: filename, Format: fileFormat, SizeBytes: int64(len(data)),
			SHA256: hex.EncodeToString(sum[:]), DwgVersion: doc.Version(), CadRelease: cadReleases[doc.Version()]},
		Warnings: []string{},
	}

	// ---------- 图纸上下文 ----------
	sheetBox := func(key int) (box, bool) {
		if key > 0 && key <= len(sheets) {
			return boxOf(sheets[key-1].Box), true
		}
		if key == 0 && !hasFrames && len(sheets) > 0 {
			return boxOf(sheets[0].Box), true
		}
		return box{}, false
	}
	sheetOf := func(x, y float64) int {
		if !hasFrames {
			return 0
		}
		for i := range sheets {
			if boxOf(sheets[i].Box).contains(x, y) {
				return i + 1
			}
		}
		return 0
	}
	// 标题栏分类区：右下角（宽 45% × 高 30%；无图框时收紧为 40% × 20%）
	zoneA := func(key int) func(x, y float64) bool {
		b, ok := sheetBox(key)
		if !ok || !b.valid() {
			return nil
		}
		fx, fy := 0.55, 0.30
		if !hasFrames {
			fx, fy = 0.60, 0.20
		}
		return func(x, y float64) bool {
			return b.contains(x, y) && x >= b.minX+fx*b.w() && y <= b.minY+fy*b.h()
		}
	}
	// 键值提取区：右下角 + 右侧竖条（竖向标题栏/会签栏），标签必须命中词典，误伤小
	zoneKV := func(key int) func(x, y float64) bool {
		b, ok := sheetBox(key)
		za := zoneA(key)
		if !ok || za == nil {
			return nil
		}
		return func(x, y float64) bool {
			return za(x, y) || (hasFrames && b.contains(x, y) && x >= b.maxX-0.12*b.w())
		}
	}

	// ---------- 顶层块参照分类：frame / title_block / symbol / content ----------
	for i := range ex.inserts {
		r := &ex.inserts[i]
		if r.depth > 0 {
			r.sheet = sheetOf(r.x, r.y)
			continue
		}
		r.sheet = sheetOf(r.x, r.y)
		if hasFrames && r.bbox.valid() {
			for si := range sheets {
				if iou(r.bbox, boxOf(sheets[si].Box)) >= 0.85 {
					r.kind, r.sheet = "frame", si+1
					break
				}
			}
		}
		if r.kind != "" {
			continue
		}
		labelTags := 0
		for _, a := range r.attrs {
			if canonicalLabel(a.Tag) != "" {
				labelTags++
			}
		}
		if labelTags >= 2 {
			r.kind = "title_block"
			continue
		}
		ref := 0.0
		if b, ok := sheetBox(r.sheet); ok {
			ref = b.diag()
		}
		if !r.bbox.valid() || (ref > 0 && r.bbox.diag() <= 0.05*ref && r.blockEnts <= 300) || (ref == 0 && r.blockEnts <= 50) {
			r.kind = "symbol"
		} else {
			r.kind = "content"
		}
	}

	// ---------- 文字来源 ----------
	om := matchOrigins(texts, ex.origins)
	resp.Texts = make([]textDTO, len(texts))
	eligible := make([]bool, len(texts))
	fromFrame := make([]bool, len(texts))
	for ti, t := range texts {
		d := textDTO{Text: t.Text, X: t.X, Y: t.Y, Height: t.Height, Rotation: t.Rotation, Layer: t.Layer, Sheet: t.Sheet}
		eligible[ti] = true
		if o := om[ti]; o >= 0 {
			org := ex.origins[o]
			switch org.role {
			case "dimension":
				d.Role, eligible[ti] = "dimension", false
			case "attdef":
				d.Role, eligible[ti] = "attdef", false
				d.Tag = strings.TrimSpace(org.tag)
				if org.insert >= 0 && org.insert < len(ex.inserts) {
					d.Block = ex.inserts[org.insert].block
				}
			case "attribute", "block_text":
				if org.insert >= 0 && org.insert < len(ex.inserts) {
					ins := ex.inserts[org.insert]
					root := ex.inserts[ins.root]
					d.Block, d.Tag = ins.block, strings.TrimSpace(org.tag)
					switch root.kind {
					case "symbol":
						d.Role, eligible[ti] = org.role, false
					case "frame", "title_block":
						fromFrame[ti] = true
					}
				}
			}
		}
		resp.Texts[ti] = d
	}

	// ---------- 按图纸分组的版面项 ----------
	itemsBy := map[int][]*item{}
	for ti, t := range texts {
		if !eligible[ti] || strings.TrimSpace(t.Text) == "" {
			continue
		}
		h := t.Height
		if h <= 0 {
			h = 1
		}
		itemsBy[t.Sheet] = append(itemsBy[t.Sheet], &item{text: t.Text, x: t.X, y: t.Y, h: h, w: estTextWidth(t.Text, h), rot: t.Rotation, ti: ti})
	}

	// ---------- 图例识别：符号块右侧同行的中文名称 ----------
	detectLegends(ex, itemsBy)
	labelOf := map[string]string{}
	for i := range ex.inserts {
		r := &ex.inserts[i]
		if r.legend && r.label != "" && labelOf[r.block] == "" {
			labelOf[r.block] = r.label
		}
	}
	for i := range ex.inserts {
		r := &ex.inserts[i]
		if r.legend {
			c := r.bbox
			itemsBy[r.sheet] = append(itemsBy[r.sheet], &item{text: "[" + r.block + "]", x: c.minX, y: r.legendY, h: r.legendH, w: c.w(), ti: -1, pseudo: true})
		}
		if r.label == "" {
			r.label = labelOf[r.block]
		}
	}

	// ---------- 版面分析 ----------
	keys := make([]int, 0, len(itemsBy))
	for k := range itemsBy {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool { // 1..n，图框外(0)放最后
		if (keys[a] == 0) != (keys[b] == 0) {
			return keys[b] == 0
		}
		return keys[a] < keys[b]
	})
	tables := 0
	for _, k := range keys {
		for _, b := range layoutItems(itemsBy[k], zoneA(k)) {
			id := len(resp.TextBlocks) + 1
			n := 0
			for _, ti := range b.tis {
				if ti < 0 {
					continue
				}
				n++
				resp.Texts[ti].TextBlock = id
				if fromFrame[ti] && b.typ == "text" {
					resp.Texts[ti].Role = "title_block"
				} else {
					resp.Texts[ti].Role = b.typ
				}
			}
			if b.table != nil {
				tables++
			}
			resp.TextBlocks = append(resp.TextBlocks, textBlockDTO{
				ID: id, Sheet: k, Type: b.typ, Title: b.title, Lines: b.lines, Table: b.table,
				Box: [4]float64{b.minX, b.minY, b.maxX, b.maxY}, TextCount: n,
			})
		}
	}

	// ---------- 块统计 ----------
	type agg struct {
		dto  blockDTO
		tags map[string]bool
		ord  []string
	}
	aggs := map[string]*agg{}
	get := func(name, kind string) *agg {
		a := aggs[name]
		if a == nil {
			a = &agg{dto: blockDTO{Name: name, Kind: kind, SheetCounts: map[int]int{}, LayerCounts: map[string]int{}}, tags: map[string]bool{}}
			aggs[name] = a
		}
		return a
	}
	topInserts := 0
	sheetBlock := map[int]map[string]int{}
	for i := range ex.inserts {
		r := &ex.inserts[i]
		name := r.block
		if name == "" {
			name = "(未命名块)"
		}
		if isSystemBlock(name) {
			continue
		}
		if r.depth > 0 {
			get(name, "symbol").dto.NestedCount++
			continue
		}
		topInserts++
		a := get(name, r.kind)
		if a.dto.Label == "" {
			a.dto.Label = labelOf[r.block]
		}
		for _, kv := range r.attrs {
			if !a.tags[kv.Tag] {
				a.tags[kv.Tag] = true
				a.ord = append(a.ord, kv.Tag)
			}
		}
		if r.legend {
			a.dto.LegendCount++
			continue
		}
		a.dto.Count++
		a.dto.SheetCounts[r.sheet]++
		a.dto.LayerCounts[r.layer]++
		if r.kind == "symbol" || r.kind == "content" {
			if sheetBlock[r.sheet] == nil {
				sheetBlock[r.sheet] = map[string]int{}
			}
			sheetBlock[r.sheet][name]++
		}
	}
	for _, a := range aggs {
		a.dto.AttributeTags = a.ord
		if len(a.dto.LayerCounts) == 0 {
			a.dto.LayerCounts = nil
		}
		resp.Blocks = append(resp.Blocks, a.dto)
	}
	sort.Slice(resp.Blocks, func(i, j int) bool {
		if resp.Blocks[i].Count != resp.Blocks[j].Count {
			return resp.Blocks[i].Count > resp.Blocks[j].Count
		}
		return resp.Blocks[i].Name < resp.Blocks[j].Name
	})

	// ---------- 块参照明细（带属性的） ----------
	resp.Inserts = []insertDTO{}
	for i := range ex.inserts {
		r := &ex.inserts[i]
		if r.depth > 0 || len(r.attrs) == 0 {
			continue
		}
		if len(resp.Inserts) >= maxInserts {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("带属性的块参照超过 %d 个，inserts 已截断（CAD_MAX_INSERTS）", maxInserts))
			break
		}
		attrs := make(map[string]string, len(r.attrs))
		for _, kv := range r.attrs {
			if _, dup := attrs[kv.Tag]; !dup {
				attrs[kv.Tag] = kv.Value
			}
		}
		resp.Inserts = append(resp.Inserts, insertDTO{
			ID: r.id, Block: r.block, Label: r.label, Kind: r.kind, Legend: r.legend,
			X: r.x, Y: r.y, Rotation: r.rot, ScaleX: r.sx, ScaleY: r.sy, Layer: r.layer, Sheet: r.sheet, Attributes: attrs,
		})
	}

	// ---------- 尺寸标注 ----------
	resp.Dimensions = []dimDTO{}
	dimBySheet := map[int]int{}
	computedDims := 0
	for _, d := range ex.dims {
		s := sheetOf(d.x, d.y)
		dimBySheet[s]++
		if d.computed {
			computedDims++
		}
		if len(resp.Dimensions) < maxDimensions {
			resp.Dimensions = append(resp.Dimensions, dimDTO{Type: d.typ, Value: round6(d.value), Text: d.text, Computed: d.computed,
				X: d.x, Y: d.y, Layer: d.layer, Sheet: s})
		}
	}
	if len(ex.dims) > maxDimensions {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("尺寸标注超过 %d 个，dimensions 已截断（CAD_MAX_DIMENSIONS）", maxDimensions))
	}

	// ---------- 图层 ----------
	textsByLayer := map[string]int{}
	sheetLayers := map[int]map[string]int{}
	for _, t := range texts {
		textsByLayer[t.Layer]++
		if sheetLayers[t.Sheet] == nil {
			sheetLayers[t.Sheet] = map[string]int{}
		}
		sheetLayers[t.Sheet][t.Layer]++
	}
	seenLayer := map[string]bool{}
	for _, lc := range doc.LayerColors {
		if lc.Name == "" || seenLayer[lc.Name] {
			continue
		}
		seenLayer[lc.Name] = true
		resp.Layers = append(resp.Layers, layerDTO{Name: lc.Name, Color: int(lc.Index),
			Entities: ex.layerEnts[lc.Name], Texts: textsByLayer[lc.Name], Inserts: ex.layerIns[lc.Name]})
	}
	sort.Slice(resp.Layers, func(i, j int) bool {
		if resp.Layers[i].Entities != resp.Layers[j].Entities {
			return resp.Layers[i].Entities > resp.Layers[j].Entities
		}
		return resp.Layers[i].Name < resp.Layers[j].Name
	})

	// ---------- 图纸 ----------
	counts := map[int]int{}
	for _, t := range texts {
		counts[t.Sheet]++
	}
	for i, s := range sheets {
		idx := i + 1
		key := idx
		tc := counts[idx]
		if !hasFrames {
			key, tc = 0, len(texts) // 无图框：整图包含全部文本（sheet 字段均为 0）
		}
		var attrs []attrKV
		for j := range ex.inserts {
			r := &ex.inserts[j]
			if r.depth == 0 && r.sheet == key && (r.kind == "frame" || r.kind == "title_block") {
				attrs = append(attrs, r.attrs...)
			}
		}
		kv := extractTitleBlock(titleBlockInput{items: itemsBy[key], attrs: attrs, inZone: zoneKV(key)})
		number, title := kv["图号"], kv["图名"]
		if s.Name != "整图" && (number == "" || title == "") {
			n, t := splitSheetName(s.Name)
			if number == "" {
				number = n
			}
			if title == "" {
				title = t
			}
		}
		scale := kv["比例"]
		if scale == "" {
			scale = findScale(itemsBy[key], zoneKV(key))
		}
		disc := kv["专业"]
		if disc == "" {
			disc = inferDiscipline(number, title)
		}
		var bcs []blockCountDTO
		for name, c := range sheetBlock[key] {
			bcs = append(bcs, blockCountDTO{Block: name, Label: labelOf[name], Count: c})
		}
		sort.Slice(bcs, func(a, b int) bool {
			if bcs[a].Count != bcs[b].Count {
				return bcs[a].Count > bcs[b].Count
			}
			return bcs[a].Block < bcs[b].Block
		})
		if len(bcs) > 100 {
			bcs = bcs[:100]
		}
		resp.Sheets = append(resp.Sheets, sheetDTO{
			Index: idx, Name: s.Name, Number: number, Title: title, Discipline: disc, Scale: scale,
			TitleBlock: kv, Box: s.Box, TextCount: tc, DimensionCount: dimBySheet[key],
			BlockCounts: nonNil(bcs), Layers: topKeys(sheetLayers[key], 20),
		})
	}

	// ---------- 统计与告警 ----------
	resp.Stats = statsDTO{
		Entities: ex.entityCount, EntityTypes: ex.entityTypes, Layers: len(resp.Layers), BlockDefs: ex.blockDefs,
		Inserts: topInserts, Texts: len(texts), Dimensions: len(ex.dims), TextBlocks: len(resp.TextBlocks),
		Tables: tables, SkippedObjects: doc.Skipped(), AttribFixed: attribFixed,
	}
	if !hasFrames {
		resp.Warnings = append(resp.Warnings, "未识别到图框，全部内容按整图处理（图号/图名可能缺失）")
	}
	if n := doc.Skipped(); n > 0 {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d 个对象解码失败已跳过，可能有少量图元/文字缺失", n))
	}
	if len(texts) == 0 {
		resp.Warnings = append(resp.Warnings, "未提取到任何文字：可能是纯图形图纸、文字已分解为线条、内容在外部参照(XREF)中，或坐标超出 ±1e7（带带号的大地坐标）")
	}
	garbled := 0
	for _, t := range texts {
		if strings.ContainsRune(t.Text, '\uFFFD') || strings.Contains(t.Text, "锟斤拷") {
			garbled++
		}
	}
	if garbled > 0 {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d 条文字疑似乱码（代码页/大字体问题），建议抽查", garbled))
	}
	if computedDims > 0 && fileFormat == "dxf" {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf("%d 个尺寸标注的数值由定义点计算得到（DXF 中标注文字不在 texts 里，请使用 dimensions）", computedDims))
	}
	if ex.truncated {
		resp.Warnings = append(resp.Warnings, "块参照数量过多，嵌套块统计不完整")
	}
	if resp.Blocks == nil {
		resp.Blocks = []blockDTO{}
	}
	if resp.TextBlocks == nil {
		resp.TextBlocks = []textBlockDTO{}
	}
	if resp.Layers == nil {
		resp.Layers = []layerDTO{}
	}

	stage.mark("enrich")
	// ---------- 渲染 ----------
	if withRender {
		results, rerr := cad.RenderAllSheets(doc, cad.RenderOptions{Width: width, FontPath: fontPath}, format)
		if rerr != nil {
			return nil, fmt.Errorf("render: %w", rerr)
		}
		// 有图框时首项固定为"整图全览"，跳过后与 sheets 序号对齐
		if len(results) > 1 && results[0].Name == "整图全览" {
			results = results[1:]
		}
		for i := range resp.Sheets {
			if i < len(results) {
				resp.Sheets[i].ImageFormat = format
				resp.Sheets[i].ImageBase64 = base64.StdEncoding.EncodeToString(results[i].Data)
				resp.Sheets[i].ImageWidth = results[i].Width
				resp.Sheets[i].PassRate = results[i].PassRate
			}
		}
		stage.mark("render")
	}
	resp.timings = stage.String()
	return resp, nil
}

// stageTimer 记录各阶段耗时，写进访问日志（定位慢文件是卡在 go-cad 解析还是增强分析）。
type stageTimer struct {
	last  time.Time
	parts []string
}

func newStageTimer() *stageTimer { return &stageTimer{last: time.Now()} }
func (s *stageTimer) mark(name string) {
	now := time.Now()
	s.parts = append(s.parts, fmt.Sprintf("%s=%dms", name, now.Sub(s.last).Milliseconds()))
	s.last = now
}
func (s *stageTimer) String() string { return strings.Join(s.parts, " ") }

// detectLegends 图例/设备表里的示意符号：符号块右侧同一行紧挨着一条中文名称，并且
// 满足其一：同一列上下还有别的符号块也带名称（图例列），或附近有"图例"二字。
// 识别为图例的实例不计入设备数量，名称回填到同名块的所有实例。
func detectLegends(ex *extraction, itemsBy map[int][]*item) {
	type cand struct {
		ins   int
		label string
		item  *item
		cx    float64
		cy    float64
		h     float64
	}
	// 每张图预筛一次可做图例名称的文字（单行、水平、含汉字、2~30 字），按中心 y 排序，
	// 再按 y 窗口二分查找 —— 避免 设备数 × 文字数 的全量扫描（大图上是秒级差异）。
	type labelIdx struct {
		items []*item
		maxH  float64
		title []*item // “图例”标题
	}
	idx := map[int]*labelIdx{}
	for sh, items := range itemsBy {
		li := &labelIdx{}
		for _, it := range items {
			if it.pseudo {
				continue
			}
			if n := runeLen(it.text); n <= 12 && strings.Contains(strings.ReplaceAll(it.text, " ", ""), "图例") {
				li.title = append(li.title, it)
			}
			if !isHorizontal(it.rot) || strings.Contains(it.text, "\n") || !hasCJK(it.text) {
				continue
			}
			if n := runeLen(it.text); n < 2 || n > 30 {
				continue
			}
			li.items = append(li.items, it)
			li.maxH = math.Max(li.maxH, it.h)
		}
		sort.Slice(li.items, func(a, b int) bool { return li.items[a].cy() < li.items[b].cy() })
		idx[sh] = li
	}
	var cands []cand
	for i := range ex.inserts {
		r := &ex.inserts[i]
		if r.depth > 0 || r.kind != "symbol" || !r.bbox.valid() {
			continue
		}
		li := idx[r.sheet]
		if li == nil || len(li.items) == 0 {
			continue
		}
		cx, cy := (r.bbox.minX+r.bbox.maxX)/2, (r.bbox.minY+r.bbox.maxY)/2
		sh := r.bbox.h()
		win := math.Max(0.6*sh, 0.8*li.maxH)
		lo := sort.Search(len(li.items), func(k int) bool { return li.items[k].cy() >= cy-win })
		var best *item
		bestDx := math.Inf(1)
		for _, it := range li.items[lo:] {
			if it.cy() > cy+win {
				break
			}
			if math.Abs(it.cy()-cy) > math.Max(0.6*sh, 0.8*it.h) {
				continue
			}
			dx := it.x - r.bbox.maxX
			if dx < -0.3*it.h || dx > math.Max(12*it.h, 3*sh) {
				continue
			}
			if dx < bestDx {
				best, bestDx = it, dx
			}
		}
		if best != nil {
			cands = append(cands, cand{ins: i, label: best.text, item: best, cx: cx, cy: cy, h: math.Max(best.h, sh)})
		}
	}
	// 图例的特征：不同的块竖向排成一列，每个右边跟一个名称；或者上方不远处有“图例”标题。
	sorted := make([]int, len(cands))
	for k := range sorted {
		sorted[k] = k
	}
	sort.Slice(sorted, func(a, b int) bool { return cands[sorted[a]].cy < cands[sorted[b]].cy })
	pos := make([]int, len(cands)) // cands 下标 -> sorted 中的位置
	for p, k := range sorted {
		pos[k] = p
	}
	for k, c := range cands {
		r := &ex.inserts[c.ins]
		match := func(o cand) bool {
			if o.ins == c.ins || ex.inserts[o.ins].block == r.block || ex.inserts[o.ins].sheet != r.sheet {
				return false
			}
			return math.Abs(o.cx-c.cx) <= math.Max(r.bbox.w(), c.h)
		}
		ok := false
		for p := pos[k] + 1; p < len(sorted) && !ok; p++ {
			o := cands[sorted[p]]
			if o.cy-c.cy > 6*c.h {
				break
			}
			ok = match(o)
		}
		for p := pos[k] - 1; p >= 0 && !ok; p-- {
			o := cands[sorted[p]]
			if c.cy-o.cy > 6*c.h {
				break
			}
			ok = match(o)
		}
		if !ok {
			for _, it := range idx[r.sheet].title {
				if dy := it.y - c.cy; dy > 0 && dy <= 40*c.h && math.Abs(it.x-c.cx) <= 40*c.h {
					ok = true
					break
				}
			}
		}
		if ok {
			r.legend, r.label = true, c.label
			r.legendY, r.legendH = c.item.y, c.item.h
		}
	}
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// dedupTexts 去掉内容、位置、图层完全相同的重复文字（R13/R2000 文件里 INSERT 的属性句柄
// 列表常重复，go-cad 会把同一属性输出两次；复制粘贴叠在一起的文字也一并去掉）。
func dedupTexts(in []cad.SheetText) []cad.SheetText {
	type key struct {
		text, layer string
		x, y        float64
	}
	seen := make(map[key]bool, len(in))
	out := in[:0]
	for _, t := range in {
		k := key{t.Text, t.Layer, math.Round(t.X*1e4) / 1e4, math.Round(t.Y*1e4) / 1e4}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	return out
}

func nonNil(b []blockCountDTO) []blockCountDTO {
	if b == nil {
		return []blockCountDTO{}
	}
	return b
}

func topKeys(m map[string]int, n int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(a, b int) bool {
		if m[keys[a]] != m[keys[b]] {
			return m[keys[a]] > m[keys[b]]
		}
		return keys[a] < keys[b]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	return keys
}
