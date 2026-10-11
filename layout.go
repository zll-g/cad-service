package main

// 版面分析：把散落在二维坐标上的文字还原成"段落 / 表格 / 图例 / 标题栏"等文本块。
//
// CAD 里一段设计说明、一张材料表，在文件中只是几十条互不相关的 TEXT。直接按 Y 排序
// 拼接时，左右两栏的内容会交错在一起，表格的行列关系也会丢失，检索和问答效果都差。
// 这里按几何关系分三步重建：
//   1. 行：基线接近、字高相近、水平间距不过大的文字归为一行（行内按 X 排序）；
//   2. 块：上下相邻、行距合理、水平范围有重叠的行归为一个文本块；
//   3. 分类：说明（标题含"说明/注"或多行编号）、表格（≥3 行且每行 ≥3 列，
//      按表头列中心对齐成二维表）、图例（行首是设备符号块）、标题栏、普通文字。
// MTEXT（多行文字）与旋转文字不参与行合并，各自成块。

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

type item struct {
	text   string
	x, y   float64 // 插入点（TEXT 为基线左端；MTEXT 为附着点）
	h, w   float64 // 字高、估算宽度
	rot    float64
	ti     int  // 在 texts 中的下标；伪文字（图例符号）为 -1
	pseudo bool // 图例符号占位 "[块名]"
}

func (it *item) cx() float64    { return it.x + it.w/2 }
func (it *item) cy() float64    { return it.y + it.h/2 }
func (it *item) right() float64 { return it.x + it.w }

type row struct {
	items      []*item
	y, h       float64
	minX, maxX float64
}

type tblock struct {
	rows                   []*row
	minX, minY, maxX, maxY float64
	typ, title             string
	lines                  []string
	table                  *tableDTO
	tis                    []int
	standalone             bool
}

var (
	notesHeadRe = regexp.MustCompile(`^.{0,12}(说明|备注|注意事项|技术要求|施工要求|设计依据)\s*[:：]?\s*$|^(注|说明|备注)\s*[:：]`)
	numberedRe  = regexp.MustCompile(`^\s*(\d{1,2}|[一二三四五六七八九十]{1,3}|[(（]\d{1,2}[)）])\s*[.、．)）]`)
	tableHeadRe = regexp.MustCompile(`序号|名称|型号|规格|单位|数量|备注|图例|编号|设备|材料|安装|做法|功率|容量|回路|用途|参数`)
)

func isHorizontal(rot float64) bool {
	r := math.Mod(rot, 360)
	if r < 0 {
		r += 360
	}
	return r <= 5 || r >= 355
}

// layoutItems 对一张图纸（或图框外区域）的文字做版面分析。
// isTitleZone 判断坐标是否在标题栏区域（可为 nil）。
func layoutItems(items []*item, isTitleZone func(x, y float64) bool) []*tblock {
	var blocks []*tblock
	var flow []*item
	for _, it := range items {
		if strings.Contains(it.text, "\n") || !isHorizontal(it.rot) {
			blocks = append(blocks, standaloneBlock(it))
			continue
		}
		flow = append(flow, it)
	}

	// ---- 1. 行 ----
	sort.Slice(flow, func(a, b int) bool {
		if flow[a].y != flow[b].y {
			return flow[a].y > flow[b].y
		}
		return flow[a].x < flow[b].x
	})
	var rows, active []*row
	for _, it := range flow {
		var best *row
		bestGap := math.Inf(1)
		keep := active[:0]
		for _, r := range active {
			tol := 0.5 * math.Min(r.h, it.h)
			if r.y-it.y > math.Max(r.h, it.h) { // 已经远在上方，不再参与
				continue
			}
			keep = append(keep, r)
			if math.Abs(r.y-it.y) > tol || math.Max(r.h, it.h)/math.Min(r.h, it.h) > 1.6 {
				continue
			}
			gap := math.Max(0, math.Max(r.minX-it.right(), it.x-r.maxX))
			if gap <= 15*it.h && gap < bestGap {
				best, bestGap = r, gap
			}
		}
		active = keep
		if best == nil {
			best = &row{y: it.y, h: it.h, minX: it.x, maxX: it.right()}
			rows = append(rows, best)
			active = append(active, best)
		}
		best.items = append(best.items, it)
		best.minX = math.Min(best.minX, it.x)
		best.maxX = math.Max(best.maxX, it.right())
	}
	for _, r := range rows {
		sort.Slice(r.items, func(a, b int) bool { return r.items[a].x < r.items[b].x })
	}

	// ---- 2. 块 ----
	sort.Slice(rows, func(a, b int) bool {
		if rows[a].y != rows[b].y {
			return rows[a].y > rows[b].y
		}
		return rows[a].minX < rows[b].minX
	})
	var open []*tblock
	for _, r := range rows {
		var best *tblock
		bestGap := math.Inf(1)
		keepOpen := open[:0]
		for _, b := range open {
			last := b.rows[len(b.rows)-1]
			hRef := math.Max(last.h, r.h)
			gap := last.y - r.y
			if gap > 3.5*hRef+1e-9 {
				continue // 关闭：后续行只会更靠下
			}
			keepOpen = append(keepOpen, b)
			if gap <= 0 || math.Max(last.h, r.h)/math.Min(last.h, r.h) > 2.2 {
				continue
			}
			margin := 2 * hRef
			if r.maxX < b.minX-margin || r.minX > b.maxX+margin {
				continue
			}
			if gap < bestGap {
				best, bestGap = b, gap
			}
		}
		open = keepOpen
		if best == nil {
			best = &tblock{minX: r.minX, maxX: r.maxX, maxY: r.y + r.h, minY: r.y}
			blocks = append(blocks, best)
			open = append(open, best)
		}
		best.rows = append(best.rows, r)
		best.minX = math.Min(best.minX, r.minX)
		best.maxX = math.Max(best.maxX, r.maxX)
		best.minY = math.Min(best.minY, r.y)
		best.maxY = math.Max(best.maxY, r.y+r.h)
	}

	// ---- 3. 分类 ----
	for _, b := range blocks {
		if !b.standalone {
			classify(b, isTitleZone)
		}
	}
	sort.SliceStable(blocks, func(a, b int) bool {
		if math.Abs(blocks[a].maxY-blocks[b].maxY) > 1e-9 {
			return blocks[a].maxY > blocks[b].maxY
		}
		return blocks[a].minX < blocks[b].minX
	})
	return blocks
}

func standaloneBlock(it *item) *tblock {
	lines := []string{}
	for _, l := range strings.Split(it.text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	nLines := float64(len(lines))
	b := &tblock{standalone: true, lines: lines, tis: []int{it.ti},
		minX: it.x, maxX: it.x + it.w, maxY: it.y + it.h, minY: it.y - (nLines-1)*it.h*1.6}
	b.typ = "text"
	if len(lines) > 0 && (notesHeadRe.MatchString(lines[0]) || countNumbered(lines) >= 2) {
		b.typ = "notes"
		if notesHeadRe.MatchString(lines[0]) {
			b.title = strings.TrimRight(lines[0], ":： ")
		}
	}
	return b
}

func countNumbered(lines []string) int {
	n := 0
	for _, l := range lines {
		if numberedRe.MatchString(l) {
			n++
		}
	}
	return n
}

// cells 合并一行内紧挨着的文字（被拆成多段 TEXT 的同一句话），返回单元格文本与中心 X。
type cell struct {
	text   string
	cx     float64
	pseudo bool
}

func rowCells(r *row) []cell {
	var out []cell
	var cur *cell
	var curRight, curLeft float64
	for _, it := range r.items {
		if cur != nil && !it.pseudo && !cur.pseudo && it.x-curRight < 0.8*it.h {
			sep := " "
			if endsCJK(cur.text) || startsCJK(it.text) {
				sep = ""
			}
			cur.text += sep + it.text
			curRight = math.Max(curRight, it.right())
			cur.cx = (curLeft + curRight) / 2
			continue
		}
		if cur != nil {
			out = append(out, *cur)
		}
		cur = &cell{text: it.text, cx: it.cx(), pseudo: it.pseudo}
		curLeft, curRight = it.x, it.right()
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

func endsCJK(s string) bool {
	r := []rune(s)
	return len(r) > 0 && r[len(r)-1] >= 0x2E80
}
func startsCJK(s string) bool {
	for _, r := range s {
		return r >= 0x2E80
	}
	return false
}

func classify(b *tblock, isTitleZone func(x, y float64) bool) {
	allCells := make([][]cell, len(b.rows))
	multi, hasPseudo := 0, false
	for i, r := range b.rows {
		allCells[i] = rowCells(r)
		if len(allCells[i]) >= 3 {
			multi++
		}
		for _, it := range r.items {
			b.tis = append(b.tis, it.ti)
			hasPseudo = hasPseudo || it.pseudo
		}
	}
	first := ""
	if len(allCells) > 0 && len(allCells[0]) > 0 {
		first = allCells[0][0].text
	}
	plain := func(sep string) []string {
		var out []string
		for _, cs := range allCells {
			parts := make([]string, len(cs))
			for i, c := range cs {
				parts[i] = c.text
			}
			out = append(out, strings.Join(parts, sep))
		}
		return out
	}
	labelCells := 0
	for _, cs := range allCells {
		for _, c := range cs {
			if canonicalLabel(c.text) != "" {
				labelCells++
			}
		}
	}

	cxm, cym := (b.minX+b.maxX)/2, (b.minY+b.maxY)/2
	switch {
	case (isTitleZone != nil && isTitleZone(cxm, cym)) || labelCells >= 3:
		b.typ = "title_block"
		b.lines = plain("  ")
	case hasPseudo || strings.Contains(strings.ReplaceAll(first, " ", ""), "图例"):
		b.typ = "legend"
		b.table = buildTable(allCells)
		b.lines = plain(" | ")
		if t := strings.ReplaceAll(first, " ", ""); strings.Contains(t, "图例") && runeLen(t) <= 12 {
			b.title = first
		}
	case multi >= 3:
		b.typ = "table"
		b.table = buildTable(allCells)
		if b.table != nil {
			b.title = b.table.Title
		}
		b.lines = plain(" | ")
	default:
		b.lines = plain("  ")
		if notesHeadRe.MatchString(first) || countNumbered(b.lines) >= 2 {
			b.typ = "notes"
			if notesHeadRe.MatchString(first) {
				b.title = strings.TrimRight(first, ":： ")
			}
		} else {
			b.typ = "text"
		}
	}
}

// buildTable 以列数最多的行作为列锚点，把每行单元格按中心 X 就近归列。
// 表头：第一条多列行且含常见表头词；它之前的单列行合并为表名。
func buildTable(rows [][]cell) *tableDTO {
	anchorRow := -1
	for i, cs := range rows {
		if len(cs) >= 2 && (anchorRow < 0 || len(cs) > len(rows[anchorRow])) {
			anchorRow = i
		}
	}
	if anchorRow < 0 {
		return nil
	}
	anchors := make([]float64, len(rows[anchorRow]))
	for i, c := range rows[anchorRow] {
		anchors[i] = c.cx
	}
	assign := func(cs []cell) []string {
		out := make([]string, len(anchors))
		for _, c := range cs {
			best, bd := 0, math.Inf(1)
			for i, a := range anchors {
				if d := math.Abs(c.cx - a); d < bd {
					best, bd = i, d
				}
			}
			if out[best] != "" {
				out[best] += " "
			}
			out[best] += c.text
		}
		return out
	}
	t := &tableDTO{}
	i := 0
	var title []string
	for ; i < len(rows) && len(rows[i]) < 2; i++ {
		for _, c := range rows[i] {
			title = append(title, c.text)
		}
	}
	t.Title = strings.Join(title, " ")
	if i < len(rows) {
		hdr := rows[i]
		hits := 0
		for _, c := range hdr {
			if !c.pseudo && tableHeadRe.MatchString(c.text) && runeLen(c.text) <= 8 {
				hits++
			}
		}
		// 至少 2 个单元格像表头词，且占一半以上（"吸顶安装"这类数据单元格只命中 1 个）
		if hits >= 2 && hits*2 >= len(hdr) {
			t.Header = assign(hdr)
			i++
		}
	}
	for ; i < len(rows); i++ {
		if len(rows[i]) == 0 {
			continue
		}
		t.Rows = append(t.Rows, assign(rows[i]))
	}
	return t
}
