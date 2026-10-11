package main

// go-cad 属性（ATTRIB）坐标重复变换问题的兼容修正。
//
// 现象：DWG/DXF 规范里 ATTRIB 的插入点、字高、旋转角已经是插入后的最终值
// （与 INSERT 平级的坐标系），但 go-cad（v0.0.0-20261001090957）在展开 INSERT 时
// 又对 ATTRIB 套了一次块变换。块参照只要不是"原点 + 1 倍比例"，属性文字就会飞走：
//   - 1:100 放大的图框，标题栏属性（图号/图名/设计/审核…）被放大 100 倍，
//     图框包围盒被撑大，相邻图框被合并成一张，图号图名识别失败；
//   - 设备块的 编号/回路 等属性落到图框外（sheet=0），无法归属到图纸。
// 用 go-cad 自带的 testdata/lw_example2018.dwg 和 ezdxf 生成的 DXF 均可复现。
//
// 修正：在调用 DetectSheets / SheetTexts / Render 之前，把每个 ATTRIB 的几何量
// 反变换到块的局部坐标系，go-cad 再正向变换一次后正好回到正确位置。
//
// 为了在 go-cad 修复该问题后不"修过头"，服务启动时用内嵌的极小 DXF 自检：
// 只有确认当前 go-cad 版本存在该问题时才启用修正。CAD_FIX_ATTRIB=auto|on|off 可强制。

import (
	_ "embed"
	"log"
	"math"
	"os"
	"strings"

	cad "github.com/unitedrhino/go-cad"
)

// selftest_attrib.dxf：块 SELFTEST 插入在 (1000,500)，属性 ATTRIB_SELFTEST 的
// 规范坐标也是 (1000,500)。存在问题的 go-cad 会把它算到 (2000,1000)。
//
//go:embed selftest_attrib.dxf
var selftestAttribDXF []byte

var attribFixEnabled bool

func initAttribFix() {
	mode := strings.ToLower(os.Getenv("CAD_FIX_ATTRIB"))
	switch mode {
	case "on", "true", "1":
		attribFixEnabled = true
		log.Printf("attrib position fix: ON (forced by CAD_FIX_ATTRIB)")
		return
	case "off", "false", "0":
		attribFixEnabled = false
		log.Printf("attrib position fix: OFF (forced by CAD_FIX_ATTRIB)")
		return
	}
	attribFixEnabled = detectAttribBug()
	if attribFixEnabled {
		log.Printf("attrib position fix: ON (self-test: this go-cad version double-transforms ATTRIB)")
	} else {
		log.Printf("attrib position fix: OFF (self-test: go-cad places ATTRIB correctly)")
	}
}

func detectAttribBug() (buggy bool) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("attrib self-test panic: %v (fix disabled)", p)
			buggy = false
		}
	}()
	doc, err := cad.ParseDXF(selftestAttribDXF)
	if err != nil {
		log.Printf("attrib self-test parse error: %v (fix disabled)", err)
		return false
	}
	for _, t := range cad.SheetTexts(doc) {
		if t.Text == "ATTRIB_SELFTEST" {
			return math.Abs(t.X-1000) > 1 || math.Abs(t.Y-500) > 1
		}
	}
	log.Printf("attrib self-test: attribute text not found (fix disabled)")
	return false
}

// fixAttribPositions 把 doc 中所有 INSERT 关联的 ATTRIB 反变换到块局部坐标。
// 每个 ATTRIB 只处理一次（R13 文件里同一属性句柄会在 Attribs 列表中重复出现）。
// 返回修正的属性数。
func fixAttribPositions(doc *cad.Document) int {
	if !attribFixEnabled || doc == nil || len(doc.Attribs) == 0 {
		return 0
	}
	done := make(map[uint64]bool, len(doc.Attribs))
	fixed := 0
	visit := func(list []any) {
		for _, e := range list {
			if kindOf(e) != "EntInsert" {
				continue
			}
			t := insertXform(e)
			if math.Abs(t.sx) < 1e-12 || math.Abs(t.sy) < 1e-12 || !finite(t.sx, t.sy, t.tx, t.ty, t.cos, t.sin) {
				continue
			}
			for _, ah := range fUints(e, "Attribs") {
				if done[ah] {
					continue
				}
				a := doc.Attribs[ah]
				if a == nil {
					continue
				}
				done[ah] = true
				// 位置：p = M⁻¹(P - t)，M = R(θ)·diag(sx, sy)
				inv := func(X, Y float64) (float64, float64) {
					dx, dy := X-t.tx, Y-t.ty
					u := dx*t.cos + dy*t.sin
					v := -dx*t.sin + dy*t.cos
					return u / t.sx, v / t.sy
				}
				a.Insertion.X, a.Insertion.Y = inv(a.Insertion.X, a.Insertion.Y)
				if (a.HAlign != 0 || a.VAlign != 0) && a.AlignPt != nil {
					a.AlignPt.X, a.AlignPt.Y = inv(a.AlignPt.X, a.AlignPt.Y)
				}
				// 方向与字高：w = M⁻¹·d，局部角 = atan2(w)，局部字高 = h·|w|
				// （go-cad 的世界字高 = |M·ŵ|·h局部 = h，正好还原）
				dx, dy := math.Cos(a.Rotation), math.Sin(a.Rotation)
				wx := (dx*t.cos + dy*t.sin) / t.sx
				wy := (-dx*t.sin + dy*t.cos) / t.sy
				if n := math.Hypot(wx, wy); n > 0 && finite(n) {
					a.Rotation = math.Atan2(wy, wx)
					a.Height *= n
				}
				fixed++
			}
		}
	}
	visit(doc.ModelSpace)
	visit(doc.PspaceSpace)
	for _, list := range doc.Blocks {
		visit(list)
	}
	return fixed
}
