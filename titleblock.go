package main

// 标题栏 / 会签栏的键值提取，以及 图号、图名、专业、比例 的规范化。
//
// 来源（按优先级）：
//   1. 图框块 / 标题栏块的属性：tag 本身就是标签（"图号"、"DWGNO"…）；
//   2. 标题栏区域内"标签 + 值"两条分开的文字：值在标签右侧同一行，或正下方；
//   3. 标签与值写在同一条文字里："比例 1:100"、"设计单位：某某院"。
// 同义标签归一到规范键（设计人→设计，绘图→制图，DWGNO→图号 …），方便下游按键检索/过滤。

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var labelSynonyms = map[string][]string{
	"工程名称":  {"工程名称", "项目名称", "工程项目", "项目", "工程", "PROJECT", "PROJECTNAME", "PROJ_NAME"},
	"子项名称":  {"子项名称", "子项", "单体名称", "单项名称", "SUBPROJECT"},
	"建设单位":  {"建设单位", "业主", "甲方", "CLIENT", "OWNER"},
	"设计单位":  {"设计单位", "设计院"},
	"图名":    {"图名", "图纸名称", "图纸名", "TITLE", "DWGNAME", "DRAWINGNAME", "DWG_TITLE", "SHEETTITLE"},
	"图号":    {"图号", "图纸编号", "图纸号", "图别图号", "DWGNO", "DRAWINGNO", "DWG_NO", "DRAWING_NO", "SHEETNO", "SHEET_NO"},
	"比例":    {"比例", "SCALE"},
	"设计":    {"设计", "设计人", "DESIGN", "DESIGNER", "DESIGNED", "DESIGNEDBY"},
	"制图":    {"制图", "绘图", "制图人", "绘图人", "DRAWN", "DRAWNBY", "DRAWER"},
	"校对":    {"校对", "校核", "CHECK", "CHECKED", "CHECKER", "CHECKEDBY"},
	"审核":    {"审核", "审核人", "REVIEW", "REVIEWED", "REVIEWER"},
	"审定":    {"审定", "审定人"},
	"批准":    {"批准", "院长", "APPROVE", "APPROVED", "APPROVER", "APPROVEDBY"},
	"专业负责人": {"专业负责人", "专业负责", "工种负责人"},
	"项目负责人": {"项目负责人", "项目负责", "工程负责人", "项目经理", "设总", "设计总负责人"},
	"日期":    {"日期", "出图日期", "设计日期", "DATE"},
	"版次":    {"版次", "版本", "版号", "修改版次", "REV", "REVISION", "VERSION"},
	"设计阶段":  {"设计阶段", "阶段", "STAGE", "PHASE"},
	"工程编号":  {"工程编号", "项目编号", "工程号", "合同号", "PROJECTNO", "PROJ_NO", "JOBNO"},
	"专业":    {"专业", "DISCIPLINE"},
}

var (
	labelToKey    = map[string]string{}
	labelsByLen   []string          // 长标签优先匹配（"设计单位"先于"设计"）
	labelsByFirst map[rune][]string // 规范化标签按首字分桶（inlineLabel 热路径）
	labelSpaceRe  = regexp.MustCompile(`[\s\x{3000}:：_\-]+`)
	scaleRe       = regexp.MustCompile(`1\s*[:：/]\s*\d+(?:\.\d+)?`)
	sheetNoLikeRe = regexp.MustCompile(`^[A-Za-z]{1,6}(?:\([A-Za-z]{1,4}\))?[-－—_]?\d{1,4}(?:[-－—_.]\d{1,3})?[A-Za-z]?$`)
)

func init() {
	for key, syns := range labelSynonyms {
		for _, s := range syns {
			n := normLabel(s)
			labelToKey[n] = key
			labelsByLen = append(labelsByLen, s)
		}
	}
	sort.Slice(labelsByLen, func(a, b int) bool { return runeLen(labelsByLen[a]) > runeLen(labelsByLen[b]) })
	labelsByFirst = map[rune][]string{}
	for _, l := range labelsByLen {
		nl := normLabel(l)
		r, _ := utf8.DecodeRuneInString(nl)
		labelsByFirst[r] = append(labelsByFirst[r], nl)
	}
}

func normLabel(s string) string {
	return strings.ToUpper(labelSpaceRe.ReplaceAllString(strings.TrimSpace(s), ""))
}

// canonicalLabel 文字本身是不是一个标签（"设 计"、"审核："、"DWG NO" 都算），返回规范键。
func canonicalLabel(s string) string {
	if runeLen(s) > 12 {
		return ""
	}
	return labelToKey[normLabel(s)]
}

// inlineLabel "比例 1:100" / "设计单位：某某院" → (规范键, 值)。
// 标签和值之间必须有分隔符（空格或冒号），避免把"设计说明"拆成 设计=说明。
func inlineLabel(s string) (string, string) {
	s = strings.TrimSpace(s)
	first, _ := utf8.DecodeRuneInString(strings.ToUpper(s))
	for _, nl := range labelsByFirst[first] {
		rest, ok := stripLabelPrefix(s, nl)
		if !ok {
			continue
		}
		trimmed := strings.TrimLeft(rest, " \t\u3000:：")
		if trimmed == rest || trimmed == "" {
			continue
		}
		return labelToKey[nl], strings.TrimSpace(trimmed)
	}
	return "", ""
}

// stripLabelPrefix 按规范化口径（忽略标签内部的空格/下划线，大小写不敏感）匹配前缀：
// "DWG NO: E-101" 匹配 DWGNO，"设 计 张伟" 匹配 设计。返回标签之后的剩余部分。
func stripLabelPrefix(s, nl string) (string, bool) {
	acc := ""
	for i, r := range s {
		if acc != "" && (r == ' ' || r == '\u3000' || r == '_' || r == '\t') {
			continue
		}
		acc += strings.ToUpper(string(r))
		if acc == nl {
			return s[i+len(string(r)):], true
		}
		if !strings.HasPrefix(nl, acc) {
			return "", false
		}
	}
	return "", false
}

type titleBlockInput struct {
	items  []*item  // 本图纸参与版面分析的文字（含图框块文字/属性）
	attrs  []attrKV // 图框块、标题栏块的属性
	inZone func(x, y float64) bool
}

func extractTitleBlock(in titleBlockInput) map[string]string {
	kv := map[string]string{}
	put := func(k, v string) {
		v = strings.TrimSpace(v)
		if k == "" || v == "" || runeLen(v) > 80 {
			return
		}
		if _, ok := kv[k]; !ok {
			kv[k] = v
		}
	}
	// 1. 属性
	for _, a := range in.attrs {
		if k := canonicalLabel(a.Tag); k != "" {
			put(k, a.Value)
		}
	}
	// 2/3. 标题栏区文字
	var zone []*item
	for _, it := range in.items {
		if !it.pseudo && in.inZone != nil && in.inZone(it.cx(), it.cy()) && !strings.Contains(it.text, "\n") {
			zone = append(zone, it)
		}
	}
	isLabel := func(it *item) bool {
		if canonicalLabel(it.text) != "" {
			return true
		}
		k, _ := inlineLabel(it.text)
		return k != ""
	}
	for _, it := range zone {
		if k, v := inlineLabel(it.text); k != "" {
			put(k, v)
			continue
		}
		k := canonicalLabel(it.text)
		if k == "" {
			continue
		}
		// 右侧同一行最近的非标签文字
		var best *item
		bestD := math.Inf(1)
		for _, v := range zone {
			if v == it || isLabel(v) || runeLen(v.text) > 60 {
				continue
			}
			hRef := math.Max(it.h, v.h)
			dx := v.x - it.right()
			if math.Abs(v.cy()-it.cy()) <= 0.8*hRef && dx > -0.3*it.h && dx <= 30*it.h && dx < bestD {
				best, bestD = v, dx
			}
		}
		if best == nil { // 正下方
			for _, v := range zone {
				if v == it || isLabel(v) || runeLen(v.text) > 60 {
					continue
				}
				hRef := math.Max(it.h, v.h)
				dy := it.y - v.y
				if dy > 0 && dy <= 2.5*hRef && math.Abs(v.x-it.x) <= 4*it.h && dy < bestD {
					best, bestD = v, dy
				}
			}
		}
		if best != nil {
			put(k, best.text)
		}
	}
	if s, ok := kv["比例"]; ok {
		if m := scaleRe.FindString(s); m != "" {
			kv["比例"] = strings.NewReplacer("：", ":", "/", ":", " ", "").Replace(m)
		}
	}
	return kv
}

// findScale 标题栏没有"比例"标签时，从区内文字里找 1:100 形式。
func findScale(items []*item, inZone func(x, y float64) bool) string {
	for _, it := range items {
		if inZone != nil && inZone(it.cx(), it.cy()) {
			if m := scaleRe.FindString(it.text); m != "" {
				return strings.NewReplacer("：", ":", "/", ":", " ", "").Replace(m)
			}
		}
	}
	return ""
}

// ---------------- 专业 ----------------

var disciplineByPrefix = map[string]string{
	"JZ": "建筑", "JS": "建筑", "J": "建筑", "A": "建筑", "AR": "建筑",
	"JG": "结构", "GS": "结构", "G": "结构", "ST": "结构",
	"GPS": "给排水", "JPS": "给排水", "SS": "给排水", "P": "给排水", "PS": "给排水", "WS": "给排水",
	"NT": "暖通", "NS": "暖通", "KT": "暖通", "HVAC": "暖通", "M": "暖通", "N": "暖通",
	"DQ": "电气", "DS": "电气", "D": "电气", "E": "电气", "EL": "电气",
	"RD": "智能化", "ZN": "智能化", "ELV": "智能化", "ZNH": "智能化",
	"XF": "消防", "FA": "消防", "FAS": "消防",
	"ZS": "装饰", "ZT": "总图",
}

var disciplineKeywords = []struct {
	re   *regexp.Regexp
	name string
}{
	{regexp.MustCompile(`消防|火灾自动报警|喷淋|消火栓|防排烟|应急照明|疏散指示`), "消防"},
	{regexp.MustCompile(`弱电|智能化|综合布线|安防|监控|门禁|广播|信息发布`), "智能化"},
	{regexp.MustCompile(`给排水|给水|排水|雨水|污水|热水`), "给排水"},
	{regexp.MustCompile(`暖通|空调|通风|采暖|供暖|新风`), "暖通"},
	{regexp.MustCompile(`电气|照明|配电|动力|防雷|接地`), "电气"},
	{regexp.MustCompile(`结构|基础|梁|板配筋|柱|剪力墙`), "结构"},
	{regexp.MustCompile(`装饰|装修|吊顶|立面装饰`), "装饰"},
	{regexp.MustCompile(`总平面|总图|竖向|管线综合`), "总图"},
	{regexp.MustCompile(`建筑|平面图|立面图|剖面图|大样|门窗`), "建筑"},
}

var prefixRe = regexp.MustCompile(`^([A-Za-z]{1,4})(?:\(([A-Za-z]{1,4})\))?`)

// inferDiscipline 图号前缀优先（XF-01 → 消防；RD(XF)-05 取括号内的 XF），否则按图名关键词。
func inferDiscipline(number, title string) string {
	if m := prefixRe.FindStringSubmatch(strings.TrimSpace(number)); m != nil {
		if m[2] != "" {
			if d, ok := disciplineByPrefix[strings.ToUpper(m[2])]; ok {
				return d
			}
		}
		if d, ok := disciplineByPrefix[strings.ToUpper(m[1])]; ok {
			return d
		}
	}
	for _, k := range disciplineKeywords {
		if k.re.MatchString(title) {
			return k.name
		}
	}
	return ""
}

// splitSheetName go-cad 的图名格式为 "图号-图名"（如 XF-01-地下一层消防平面图）。
func splitSheetName(name string) (number, title string) {
	parts := strings.Split(name, "-")
	for i := len(parts) - 1; i >= 1; i-- {
		no := strings.Join(parts[:i], "-")
		if sheetNoLikeRe.MatchString(no) {
			return no, strings.Join(parts[i:], "-")
		}
	}
	if sheetNoLikeRe.MatchString(name) {
		return name, ""
	}
	return "", name
}
