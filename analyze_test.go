package main

// 回归测试：go test ./...（需要仓库里的 ../samples；Docker 构建上下文里没有 samples，会自动跳过）

import (
	"math"
	"os"
	"strings"
	"testing"

	cad "github.com/unitedrhino/go-cad"
)

func loadSample(t *testing.T, name string) *parseResp {
	t.Helper()
	data, err := os.ReadFile("../samples/" + name)
	if err != nil {
		t.Skipf("sample not available: %v", err)
	}
	r, err := analyze(name, data, false, "png", 0)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return r
}

func TestMain(m *testing.M) {
	initAttribFix()
	os.Exit(m.Run())
}

func TestAttribSelfTest(t *testing.T) {
	doc, err := cad.ParseDXF(selftestAttribDXF)
	if err != nil {
		t.Fatal(err)
	}
	if n := fixAttribPositions(doc); attribFixEnabled && n != 1 {
		t.Fatalf("fixed %d attribs, want 1", n)
	}
	for _, st := range cad.SheetTexts(doc) {
		if st.Text == "ATTRIB_SELFTEST" {
			if math.Abs(st.X-1000) > 1e-6 || math.Abs(st.Y-500) > 1e-6 || math.Abs(st.Height-2) > 1e-6 {
				t.Fatalf("attrib after fix at (%v,%v) h=%v, want (1000,500) h=2", st.X, st.Y, st.Height)
			}
			return
		}
	}
	t.Fatal("ATTRIB_SELFTEST not found")
}

func TestProductionSample(t *testing.T) {
	r := loadSample(t, "fire_alarm_production_sample.dxf")
	if r.SchemaVersion != 2 || !r.HasFrames || len(r.Sheets) != 2 {
		t.Fatalf("schema=%d frames=%v sheets=%d, want 2/true/2", r.SchemaVersion, r.HasFrames, len(r.Sheets))
	}
	s1, s2 := r.Sheets[0], r.Sheets[1]
	want1 := map[string]string{"图号": "XF-01", "图名": "地下一层火灾自动报警平面图", "设计": "张伟", "校对": "王芳",
		"审核": "李强", "日期": "2026.09", "比例": "1:100", "工程名称": "某某科技园1#楼"}
	for k, v := range want1 {
		if s1.TitleBlock[k] != v {
			t.Errorf("sheet1 titleBlock[%s]=%q want %q", k, s1.TitleBlock[k], v)
		}
	}
	if s1.Number != "XF-01" || s1.Title != "地下一层火灾自动报警平面图" || s1.Discipline != "消防" || s1.Scale != "1:100" {
		t.Errorf("sheet1 number/title/discipline/scale = %q/%q/%q/%q", s1.Number, s1.Title, s1.Discipline, s1.Scale)
	}
	// 标题栏是普通文字（标签与值分开写 + "比例 1:150" 写在一起）
	if s2.Number != "XF-02" || s2.Title != "火灾自动报警系统图" || s2.Scale != "1:150" || s2.TitleBlock["设计"] != "张伟" || s2.TitleBlock["审核"] != "李强" {
		t.Errorf("sheet2 = %q/%q/%q titleBlock=%v", s2.Number, s2.Title, s2.Scale, s2.TitleBlock)
	}
	// 设备数量：排除图例里的示意符号；名称来自图例
	got := map[string]blockCountDTO{}
	for _, b := range s1.BlockCounts {
		got[b.Block] = b
	}
	for blk, want := range map[string]struct {
		n     int
		label string
	}{"FA-SD": {12, "感烟探测器"}, "FA-MCP": {4, "手动报警按钮"}, "FA-ALARM": {3, "声光警报器"}} {
		if got[blk].Count != want.n || got[blk].Label != want.label {
			t.Errorf("blockCounts[%s] = %+v, want %d %s", blk, got[blk], want.n, want.label)
		}
	}
	// 表格、图例、说明
	var table, legend, notes *textBlockDTO
	for i := range r.TextBlocks {
		b := &r.TextBlocks[i]
		switch {
		case b.Type == "table" && b.Sheet == 1:
			table = b
		case b.Type == "legend":
			legend = b
		case b.Type == "notes" && b.Sheet == 1:
			notes = b
		}
	}
	if table == nil || table.Table == nil || table.Title != "主要设备材料表" || len(table.Table.Header) != 6 || len(table.Table.Rows) != 4 {
		t.Fatalf("material table not reconstructed: %+v", table)
	}
	if row := table.Table.Rows[3]; strings.Join(row, "|") != "4|报警总线|WDZN-RYJS-2x1.5|m|860|穿SC20" {
		t.Errorf("table row 4 = %v", row)
	}
	if legend == nil || len(legend.Table.Rows) != 3 || legend.Table.Rows[0][0] != "[FA-SD]" || legend.Table.Rows[0][1] != "感烟探测器" {
		t.Errorf("legend = %+v", legend)
	}
	if notes == nil || notes.Title != "设计说明" || len(notes.Lines) != 5 {
		t.Errorf("notes = %+v", notes)
	}
	// 属性明细
	n := 0
	for _, in := range r.Inserts {
		if in.Block == "FA-SD" && !in.Legend {
			n++
			if in.Sheet != 1 || in.Attributes["编号"] == "" || in.Label != "感烟探测器" {
				t.Errorf("insert %+v", in)
			}
		}
	}
	if n != 12 {
		t.Errorf("FA-SD inserts with attributes = %d, want 12", n)
	}
	// 尺寸标注（DXF：由定义点计算）
	if len(r.Dimensions) != 4 || r.Dimensions[0].Value != 7200 || r.Dimensions[3].Text != "约6000(现场核实)" {
		t.Errorf("dimensions = %+v", r.Dimensions)
	}
	// 设备属性文字不能混进正文；也不能落到图框外
	for _, tx := range r.Texts {
		if tx.Role == "attribute" && tx.Sheet != 1 {
			t.Errorf("attribute text outside sheet: %+v", tx)
		}
	}
}

func TestLegacySample(t *testing.T) {
	r := loadSample(t, "fire_weak_current_sample.dxf")
	if len(r.Sheets) != 2 || r.Sheets[0].Name != "XF-01-地下一层消防平面图" || r.Sheets[1].Name != "XF-02-首层弱电系统图" {
		t.Fatalf("sheets = %+v", r.Sheets)
	}
	if len(r.Texts) != 41 {
		t.Errorf("texts = %d, want 41 (same as schema v1)", len(r.Texts))
	}
	if r.Sheets[0].TitleBlock["设计单位"] != "某某建筑设计研究院" || r.Sheets[0].Scale != "1:100" {
		t.Errorf("titleBlock = %v", r.Sheets[0].TitleBlock)
	}
}

func TestHelpers(t *testing.T) {
	for in, want := range map[string][2]string{
		"比例 1:100":      {"比例", "1:100"},
		"设计单位：某某院":      {"设计单位", "某某院"},
		"设计说明：":         {"", ""},
		"项目负责人":         {"", ""},
		"DWG NO: E-101": {"图号", "E-101"},
	} {
		k, v := inlineLabel(in)
		if k != want[0] || v != want[1] {
			t.Errorf("inlineLabel(%q) = %q,%q want %v", in, k, v, want)
		}
	}
	if canonicalLabel("设 计") != "设计" || canonicalLabel("审核：") != "审核" || canonicalLabel("绘图") != "制图" {
		t.Error("canonicalLabel synonyms")
	}
	for in, want := range map[string][2]string{
		"XF-01-地下一层消防平面图":     {"XF-01", "地下一层消防平面图"},
		"RD(XF)-05-地下一层消防平面图": {"RD(XF)-05", "地下一层消防平面图"},
		"XF-01":               {"XF-01", ""},
		"图 3":                 {"", "图 3"},
	} {
		n, ti := splitSheetName(in)
		if n != want[0] || ti != want[1] {
			t.Errorf("splitSheetName(%q) = %q,%q", in, n, ti)
		}
	}
	if inferDiscipline("RD(XF)-05", "") != "消防" || inferDiscipline("", "首层照明平面图") != "电气" || inferDiscipline("DQ-03", "") != "电气" {
		t.Error("inferDiscipline")
	}
	if got := cleanCadText(`{\fSimSun|b1;设计说明：}\P1. 管径%%c25`); got != "设计说明：\n1. 管径φ25" {
		t.Errorf("cleanCadText = %q", got)
	}
	if formatMeasure(7200.0000001, "linear") != "7200" || formatMeasure(1504.6794, "aligned") != "1504.68" || formatMeasure(12, "radius") != "R12" {
		t.Error("formatMeasure")
	}
}
