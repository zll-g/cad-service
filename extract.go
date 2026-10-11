package main

// 遍历 go-cad 文档模型，提取 SheetTexts 之外的结构化信息：
//   - 块参照（INSERT）：块名、世界坐标、包围盒、属性（ATTRIB tag=value）、嵌套层级；
//   - 每条文字的"来源"（属性 / 块内文字 / 标注文字），用于后续区分设备标注与正文；
//   - 尺寸标注：实测值、显示文字（含文字替代）、位置；
//   - 图层、实体类型统计。
//
// 遍历规则（顶层集合、嵌套深度、环检测、变换公式）与 go-cad Tessellator 保持一致，
// 这样算出来的文字坐标与 cad.SheetTexts 的坐标完全相同，可以直接按坐标对齐。

import (
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	cad "github.com/unitedrhino/go-cad"
)

const (
	walkMaxDepth   = 12      // 与 go-cad Tessellator.maxDepth 一致
	walkBudget     = 2000000 // 与 go-cad Tessellator.Budget 一致
	walkMaxInserts = 500000  // 内部记录的块参照上限（含嵌套）
)

type box struct{ minX, minY, maxX, maxY float64 }

func emptyBox() box { return box{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)} }
func (b box) valid() bool {
	return b.minX <= b.maxX && b.minY <= b.maxY && finite(b.minX, b.minY, b.maxX, b.maxY)
}
func (b *box) add(x, y float64) {
	if !plausible(x, y) {
		return
	}
	b.minX, b.minY = math.Min(b.minX, x), math.Min(b.minY, y)
	b.maxX, b.maxY = math.Max(b.maxX, x), math.Max(b.maxY, y)
}
func (b box) w() float64      { return b.maxX - b.minX }
func (b box) h() float64      { return b.maxY - b.minY }
func (b box) diag() float64   { return math.Hypot(b.w(), b.h()) }
func (b box) arr() [4]float64 { return [4]float64{b.minX, b.minY, b.maxX, b.maxY} }
func (b box) contains(x, y float64) bool {
	return x >= b.minX && x <= b.maxX && y >= b.minY && y <= b.maxY
}
func boxOf(a [4]float64) box { return box{a[0], a[1], a[2], a[3]} }

func iou(a, b box) float64 {
	ix := math.Min(a.maxX, b.maxX) - math.Max(a.minX, b.minX)
	iy := math.Min(a.maxY, b.maxY) - math.Max(a.minY, b.minY)
	if ix <= 0 || iy <= 0 {
		return 0
	}
	inter := ix * iy
	return inter / (a.w()*a.h() + b.w()*b.h() - inter)
}

// ---------------- 结果结构（内部） ----------------

type insertRec struct {
	id          int
	block       string
	blockHandle uint64
	x, y        float64 // 世界坐标插入点
	rot         float64 // 度
	sx, sy      float64
	layer       string
	depth       int
	root        int // 顶层祖先 insert id（顶层自身 = 自己）
	bbox        box // 世界包围盒（可能无效）
	attrs       []attrKV
	blockEnts   int
	sheet       int
	kind        string // frame | symbol | content（顶层才判定）
	legend      bool
	label       string  // 图例中识别出的名称
	legendY     float64 // 图例名称文字的基线与字高（伪文字"[块名]"对齐到同一行）
	legendH     float64
}

type attrKV struct{ Tag, Value string }

// textOrigin 一条文字的来源（与 SheetTexts 按坐标 + 内容对齐）。
type textOrigin struct {
	text   string
	x, y   float64
	role   string // attribute | block_text | dimension | attdef
	tag    string
	insert int // 所属 insert id（顶层祖先通过 inserts[insert].root 取）
}

type dimRec struct {
	typ      string
	value    float64
	text     string
	x, y     float64
	layer    string
	computed bool
	sheet    int
}

type extraction struct {
	inserts     []insertRec
	origins     []textOrigin
	dims        []dimRec
	entityTypes map[string]int
	entityCount int
	layerEnts   map[string]int
	layerIns    map[string]int
	blockDefs   int
	truncated   bool
}

// blockName 块定义句柄 → 块名（DWG/DXF/JSON 三种来源都登记在 BLOCK_HEADER 内部对象）。
func blockName(doc *cad.Document, h uint64) string {
	if g := doc.InternalObjs[h]; g != nil {
		if s, ok := g.Field("name").(string); ok {
			return s
		}
	}
	return ""
}

func layerName(doc *cad.Document, h uint64) string { return doc.LayerColors[h].Name }

// ---------------- 块定义局部包围盒（判定图框 / 小符号用） ----------------

type bboxCache struct {
	doc  *cad.Document
	memo map[uint64]box
	busy map[uint64]bool
}

func (c *bboxCache) block(h uint64, depth int) box {
	if b, ok := c.memo[h]; ok {
		return b
	}
	if c.busy[h] || depth > 4 {
		return emptyBox()
	}
	c.busy[h] = true
	b := emptyBox()
	for _, e := range c.doc.Blocks[h] {
		c.addEntity(&b, e, identity(), depth)
	}
	delete(c.busy, h)
	c.memo[h] = b
	return b
}

func (c *bboxCache) addEntity(b *box, e any, t xform, depth int) {
	addPt := func(x, y float64) { b.add(t.apply(x, y)) }
	switch kindOf(e) {
	case "EntLine":
		if x, y, ok := fPoint(e, "Start"); ok {
			addPt(x, y)
		}
		if x, y, ok := fPoint(e, "End"); ok {
			addPt(x, y)
		}
	case "EntLwPolyline":
		vs := field(e, "Vertices")
		if vs.IsValid() && vs.Kind() == reflect.Slice {
			for i := 0; i < vs.Len(); i++ {
				if x, y, ok := pointOf(vs.Index(i)); ok {
					addPt(x, y)
				}
			}
		}
	case "EntPolyline2d", "EntPolyline3d":
		for _, vh := range fUints(e, "OwnedHandles") {
			if v := c.doc.ByHandle[vh]; v != nil {
				if x, y, ok := fPoint(v, "Position"); ok {
					addPt(x, y)
				}
			}
		}
	case "EntCircle", "EntArc":
		if x, y, ok := fPoint(e, "Center"); ok {
			r := fFloat(e, "Radius")
			addPt(x-r, y-r)
			addPt(x+r, y+r)
		}
	case "EntSolid":
		for _, n := range []string{"P1", "P2", "P3", "P4"} {
			if x, y, ok := fPoint(e, n); ok {
				addPt(x, y)
			}
		}
	case "EntText", "EntAttrib", "EntMText":
		x, y := textAnchor(e)
		h := fFloat(e, "Height")
		if h == 0 {
			h = fFloat(e, "TextHeight")
		}
		addPt(x, y)
		addPt(x+estTextWidth(fString(e, "Text"), h), y+h)
	case "EntPoint":
		if x, y, ok := fPoint(e, "Location"); ok {
			addPt(x, y)
		}
	case "EntInsert":
		inner := c.block(fUint(e, "BlockHeader"), depth+1)
		if inner.valid() {
			ct := t.compose(insertXform(e))
			for _, p := range [][2]float64{{inner.minX, inner.minY}, {inner.maxX, inner.minY}, {inner.minX, inner.maxY}, {inner.maxX, inner.maxY}} {
				b.add(ct.apply(p[0], p[1]))
			}
		}
	}
}

// estTextWidth 文字宽度估算：CJK/全角按 1.0 倍字高，其余 0.6 倍。
func estTextWidth(s string, h float64) float64 {
	w := 0.0
	maxW := 0.0
	for _, r := range s {
		if r == '\n' {
			maxW = math.Max(maxW, w)
			w = 0
			continue
		}
		if r >= 0x2E80 || (r >= 0xFF00 && r <= 0xFFEF) {
			w += 1.0
		} else {
			w += 0.6
		}
	}
	return math.Max(maxW, w) * h
}

// ---------------- 主遍历 ----------------

func extract(doc *cad.Document, maxInserts int) *extraction {
	ex := &extraction{
		entityTypes: map[string]int{},
		layerEnts:   map[string]int{},
		layerIns:    map[string]int{},
	}
	bc := &bboxCache{doc: doc, memo: map[uint64]box{}, busy: map[uint64]bool{}}
	budget := walkBudget
	var path []uint64

	var walk func(list []any, t xform, depth int, parent, root int)
	walk = func(list []any, t xform, depth int, parent, root int) {
		for _, e := range list {
			if depth > walkMaxDepth || budget <= 0 {
				return
			}
			budget--
			k := kindOf(e)
			if depth == 0 {
				ex.entityCount++
				ex.entityTypes[entTypeName(e)]++
				ex.layerEnts[layerName(doc, fUint(e, "Layer"))]++
			}
			switch k {
			case "EntInsert":
				bh := fUint(e, "BlockHeader")
				cyc := false
				for _, h := range path {
					if h == bh {
						cyc = true
						break
					}
				}
				if cyc {
					continue
				}
				child := t.compose(insertXform(e))
				px, py := child.tx, child.ty
				rec := insertRec{
					id: len(ex.inserts), block: blockName(doc, bh), blockHandle: bh,
					x: px, y: py, rot: child.rotationDeg(), sx: child.sx, sy: child.sy,
					layer: layerName(doc, fUint(e, "Layer")), depth: depth, root: root,
					blockEnts: len(doc.Blocks[bh]),
				}
				if depth == 0 {
					rec.root = rec.id
					ex.layerIns[rec.layer]++
				}
				if lb := bc.block(bh, 0); lb.valid() {
					rec.bbox = emptyBox()
					for _, p := range [][2]float64{{lb.minX, lb.minY}, {lb.maxX, lb.minY}, {lb.minX, lb.maxY}, {lb.maxX, lb.maxY}} {
						rec.bbox.add(child.apply(p[0], p[1]))
					}
				} else {
					rec.bbox = emptyBox()
				}
				// 属性：坐标口径与 go-cad 展开一致（修正启用时属性已是块局部坐标 → 用 child 变换）
				for _, ah := range fUints(e, "Attribs") {
					a := doc.Attribs[ah]
					if a == nil {
						continue
					}
					val := cleanCadText(a.Text)
					rec.attrs = append(rec.attrs, attrKV{Tag: strings.TrimSpace(a.Tag), Value: val})
					ax, ay := textAnchor(a)
					var wx, wy float64
					if attribFixEnabled {
						wx, wy = child.apply(ax, ay)
					} else {
						wx, wy = t.apply(ax, ay)
					}
					ex.origins = append(ex.origins, textOrigin{text: val, x: wx, y: wy, role: "attribute", tag: a.Tag, insert: rec.id})
				}
				// 属性列表里同一句柄可能重复（R13），去重
				rec.attrs = dedupAttrs(rec.attrs)
				if len(ex.inserts) < walkMaxInserts { // 嵌套块参照可能非常多，内部记录也设上限
					ex.inserts = append(ex.inserts, rec)
				} else {
					ex.truncated = true
					continue
				}
				r := rec.root
				path = append(path, bh)
				walk(doc.Blocks[bh], child, depth+1, rec.id, r)
				path = path[:len(path)-1]
			case "EntText", "EntMText":
				if depth > 0 {
					x, y := textAnchor(e)
					wx, wy := t.apply(x, y)
					ex.origins = append(ex.origins, textOrigin{text: cleanCadText(fString(e, "Text")), x: wx, y: wy, role: "block_text", insert: parent})
				}
			case "EntAttrib":
				// 块定义里的 EntAttrib 只可能是 ATTDEF（ATTRIB 挂在 INSERT 上，不在块定义列表里）；
				// 模型空间里也可能残留孤立 ATTDEF。AutoCAD 在块参照中不显示 ATTDEF（只显示 ATTRIB 值），
				// 但 go-cad 展开时会把 ATTDEF 的默认值当文字输出，这里标记出来，避免污染正文。
				if depth > 0 || fString(e, "TypeName") == "ATTDEF" {
					x, y := textAnchor(e)
					wx, wy := t.apply(x, y)
					ex.origins = append(ex.origins, textOrigin{text: cleanCadText(fString(e, "Text")), x: wx, y: wy, role: "attdef", tag: fString(e, "Tag"), insert: parent})
				}
			case "EntDimension":
				if depth == 0 {
					d := dimension(doc, e)
					ex.dims = append(ex.dims, d)
					// 标注匿名块里的文字（DWG 路径有；DXF 路径 go-cad 不关联匿名块）
					if ab := fUint(e, "AnonymousBlock"); ab != 0 {
						for _, inner := range doc.Blocks[ab] {
							if kk := kindOf(inner); kk == "EntText" || kk == "EntMText" {
								x, y := textAnchor(inner)
								wx, wy := t.apply(x, y)
								ex.origins = append(ex.origins, textOrigin{text: cleanCadText(fString(inner, "Text")), x: wx, y: wy, role: "dimension", insert: -1})
							}
						}
					}
				}
			}
		}
	}
	walk(doc.ModelSpace, identity(), 0, -1, -1)
	walk(doc.PspaceSpace, identity(), 0, -1, -1)

	for h := range doc.Blocks {
		if n := blockName(doc, h); n != "" && !isSystemBlock(n) {
			ex.blockDefs++
		}
	}
	return ex
}

func dedupAttrs(in []attrKV) []attrKV {
	if len(in) < 2 {
		return in
	}
	seen := map[attrKV]bool{}
	out := in[:0]
	for _, a := range in {
		if seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// isSystemBlock 模型/图纸空间块、标注(*D)、填充(*X)、表格(*T)、组(*A) 等匿名块。
// 注意 *U 是动态块的匿名实例，属于真实设备块，不过滤。
func isSystemBlock(name string) bool {
	u := strings.ToUpper(name)
	if strings.HasPrefix(u, "*MODEL_SPACE") || strings.HasPrefix(u, "*PAPER_SPACE") || strings.HasPrefix(u, "$MODEL_SPACE") || strings.HasPrefix(u, "$PAPER_SPACE") {
		return true
	}
	if len(u) >= 2 && u[0] == '*' {
		switch u[1] {
		case 'D', 'X', 'T', 'A', 'E':
			return true
		}
	}
	return false
}

// ---------------- 尺寸标注 ----------------

var dimTypeByCode = map[uint64]string{0: "linear", 1: "aligned", 2: "angular", 3: "diameter", 4: "radius", 5: "angular", 6: "ordinate"}
var dimTypeByName = map[string]string{
	"DIM_LINEAR": "linear", "DIM_ALIGNED": "aligned", "DIM_ANG2LN": "angular", "DIM_ANG3PT": "angular",
	"DIM_DIAMETER": "diameter", "DIM_RADIUS": "radius", "DIM_ORDINATE": "ordinate",
	"ARC_DIMENSION": "arc", "LARGE_RADIAL_DIMENSION": "radius",
}

func dimension(doc *cad.Document, e any) dimRec {
	d := dimRec{layer: layerName(doc, fUint(e, "Layer"))}
	if t, ok := dimTypeByName[fString(e, "TypeName")]; ok {
		d.typ = t
	} else {
		d.typ = dimTypeByCode[fUint(e, "DimFlag")&7]
		if d.typ == "" {
			d.typ = "linear"
		}
	}
	d.x, d.y, _ = fPoint(e, "TextMidpoint")
	d.value = fFloat(e, "ActualMeasurement")
	if d.value == 0 || !finite(d.value) {
		// R13 文件与多数 DXF 不存实测值：按定义点计算
		x13, y13, _ := fPoint(e, "Point13")
		x14, y14, _ := fPoint(e, "Point14")
		x10, y10, _ := fPoint(e, "Point10")
		x15, y15, has15 := fPoint(e, "Point15")
		has15 = has15 && fBool(e, "HasPoint15")
		switch d.typ {
		case "linear":
			r := fFloat(e, "DimRotation")
			d.value = math.Abs((x14-x13)*math.Cos(r) + (y14-y13)*math.Sin(r))
			d.computed = true
		case "aligned":
			d.value = math.Hypot(x14-x13, y14-y13)
			d.computed = true
		case "radius", "diameter":
			if has15 {
				d.value = math.Hypot(x15-x10, y15-y10)
				d.computed = true
			}
		case "ordinate":
			if fUint(e, "DimFlag")&64 != 0 {
				d.value = math.Abs(x13 - x10)
			} else {
				d.value = math.Abs(y13 - y10)
			}
			d.computed = true
		}
	}
	if d.typ == "angular" && !d.computed && d.value != 0 {
		d.value = d.value * 180 / math.Pi // DWG 角度标注实测值为弧度
	}
	user := fString(e, "UserText")
	measured := formatMeasure(d.value, d.typ)
	switch {
	case user == " ":
		d.text = "" // 单个空格 = 隐藏标注文字
	case strings.TrimSpace(user) == "" || strings.TrimSpace(user) == "<>":
		d.text = measured
	case strings.Contains(user, "<>"):
		d.text = cleanCadText(strings.ReplaceAll(user, "<>", measured))
	default:
		d.text = cleanCadText(user)
	}
	return d
}

func formatMeasure(v float64, typ string) string {
	if v == 0 || !finite(v) {
		return ""
	}
	var s string
	if math.Abs(v-math.Round(v)) < 0.005 {
		s = strconv.FormatFloat(math.Round(v), 'f', 0, 64)
	} else {
		s = strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 2, 64), "0"), ".")
	}
	switch typ {
	case "radius":
		return "R" + s
	case "diameter":
		return "φ" + s
	case "angular":
		return s + "°"
	}
	return s
}

// ---------------- 文本清洗 ----------------

var (
	mtextCode   = regexp.MustCompile(`\\[ACcFfHhLlOoQqTtWwKk][^;\\{}]*;?`)
	mtextStack  = regexp.MustCompile(`\\S([^;^/#]*)[\^/#]([^;]*);`)
	mtextSimple = regexp.MustCompile(`\\[LlOoKk]`)
	wsRun       = regexp.MustCompile(`[ \t\x{3000}]+`)
)

// cleanCadText 去掉 MTEXT 格式码、翻译 %% 特殊码（属性值、标注替代文字用；
// SheetTexts 输出的文字已由 go-cad 处理过）。
func cleanCadText(s string) string {
	if s == "" {
		return s
	}
	s = strings.ReplaceAll(s, `\P`, "\n")
	s = strings.ReplaceAll(s, `\~`, " ")
	s = mtextStack.ReplaceAllString(s, "$1/$2")
	s = mtextCode.ReplaceAllString(s, "")
	s = mtextSimple.ReplaceAllString(s, "")
	s = strings.NewReplacer("{", "", "}", "", `\\`, `\`,
		"%%c", "φ", "%%C", "φ", "%%d", "°", "%%D", "°", "%%p", "±", "%%P", "±", "%%%", "%").Replace(s)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(wsRun.ReplaceAllString(l, " "))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// ---------------- 文字来源对齐 ----------------

// matchOrigins 为每条 SheetText 找到来源（坐标几乎重合，内容优先相同）。
// 返回与 texts 等长的 origin 下标（-1 = 模型空间直接文字）。
func matchOrigins(texts []cad.SheetText, origins []textOrigin) []int {
	out := make([]int, len(texts))
	for i := range out {
		out[i] = -1
	}
	if len(origins) == 0 {
		return out
	}
	idx := make([]int, len(origins))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return origins[idx[a]].x < origins[idx[b]].x })
	used := make([]bool, len(origins))
	for ti, t := range texts {
		tol := math.Max(t.Height*0.1, 1e-6*math.Max(1, math.Abs(t.X)+math.Abs(t.Y)))
		lo := sort.Search(len(idx), func(k int) bool { return origins[idx[k]].x >= t.X-tol })
		best, bestScore := -1, math.Inf(1)
		for k := lo; k < len(idx) && origins[idx[k]].x <= t.X+tol; k++ {
			o := origins[idx[k]]
			if used[idx[k]] || math.Abs(o.y-t.Y) > tol {
				continue
			}
			// 内容必须一致（只比较字母/数字/汉字，忽略格式码、空白、%% 转义差异）。
			// 不能放宽：ATTDEF 默认值为空时不产生文字，但它的位置常与模型空间里手写的
			// 标题栏值重合，放宽会把正文误判成 ATTDEF。
			if normForMatch(o.text) != normForMatch(t.Text) {
				continue
			}
			score := math.Abs(o.x-t.X) + math.Abs(o.y-t.Y)
			if score < bestScore {
				best, bestScore = idx[k], score
			}
		}
		if best >= 0 {
			used[best] = true
			out[ti] = best
		}
	}
	return out
}

func normForMatch(s string) string {
	s = cleanCadText(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	return b.String()
}
