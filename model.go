package main

// /v1/parse 响应结构（schemaVersion=2）。
// v1 的字段（source/dwgVersion/hasFrames/sheets/texts/elapsedMs）全部保留且含义不变，
// 只做增量扩展，老客户端忽略未知字段即可继续工作。

type parseResp struct {
	SchemaVersion int            `json:"schemaVersion"`
	Source        string         `json:"source"`
	DwgVersion    string         `json:"dwgVersion"`
	HasFrames     bool           `json:"hasFrames"` // false 表示未识别到图框，整张图作为一个单元
	File          fileDTO        `json:"file"`
	Stats         statsDTO       `json:"stats"`
	Sheets        []sheetDTO     `json:"sheets"`
	TextBlocks    []textBlockDTO `json:"textBlocks"` // 版面分析后的文本块（说明/表格/图例/标题栏/普通文字）
	Blocks        []blockDTO     `json:"blocks"`     // 块（设备/符号）统计，按数量降序
	Inserts       []insertDTO    `json:"inserts"`    // 带属性的块参照明细（设备编号、回路等）
	Dimensions    []dimDTO       `json:"dimensions"` // 尺寸标注
	Layers        []layerDTO     `json:"layers"`
	Texts         []textDTO      `json:"texts"` // 原始文字（v1 字段 + role/block/tag/textBlock）
	Warnings      []string       `json:"warnings"`
	ElapsedMs     int64          `json:"elapsedMs"`

	timings string // 各阶段耗时，只写日志不返回
}

type fileDTO struct {
	Name       string `json:"name"`
	Format     string `json:"format"` // dwg | dxf
	SizeBytes  int64  `json:"sizeBytes"`
	SHA256     string `json:"sha256"` // 可作为文档 ID：重复上传去重、增量更新
	DwgVersion string `json:"dwgVersion"`
	CadRelease string `json:"cadRelease"` // AC1024 → "AutoCAD 2010-2012"
}

type statsDTO struct {
	Entities       int            `json:"entities"` // 顶层实体数（不含块内）
	EntityTypes    map[string]int `json:"entityTypes"`
	Layers         int            `json:"layers"`
	BlockDefs      int            `json:"blockDefs"`
	Inserts        int            `json:"inserts"` // 顶层块参照数
	Texts          int            `json:"texts"`
	Dimensions     int            `json:"dimensions"`
	TextBlocks     int            `json:"textBlocks"`
	Tables         int            `json:"tables"`
	SkippedObjects int            `json:"skippedObjects"` // go-cad 解码失败跳过的对象数
	AttribFixed    int            `json:"attribFixed"`    // 修正坐标的属性数（见 cadfix.go）
}

type sheetDTO struct {
	Index          int               `json:"index"`  // 1-based，与 texts[].sheet 对齐
	Name           string            `json:"name"`   // go-cad 识别的图名（"图号-图名" > 图号 > 标题栏最大字号文本 > ...）
	Number         string            `json:"number"` // 图号
	Title          string            `json:"title"`  // 图名
	Discipline     string            `json:"discipline,omitempty"`
	Scale          string            `json:"scale,omitempty"`
	TitleBlock     map[string]string `json:"titleBlock"` // 标题栏键值：工程名称/设计/校对/审核/日期/版次…
	Box            [4]float64        `json:"box"`        // 世界坐标 {minX,minY,maxX,maxY}
	TextCount      int               `json:"textCount"`  // 归属到该图框的文本数
	DimensionCount int               `json:"dimensionCount"`
	BlockCounts    []blockCountDTO   `json:"blockCounts"` // 本图设备/符号数量（已排除图例、图框）
	Layers         []string          `json:"layers"`      // 本图有文字的图层（按文字数降序）
	ImageFormat    string            `json:"imageFormat,omitempty"`
	ImageBase64    string            `json:"imageBase64,omitempty"`
	ImageWidth     int               `json:"imageWidth,omitempty"`
	PassRate       float64           `json:"passRate,omitempty"` // 文字清晰达标率
}

type blockCountDTO struct {
	Block string `json:"block"`
	Label string `json:"label,omitempty"` // 图例里的名称，如 FA-SD → 感烟探测器
	Count int    `json:"count"`
}

type textBlockDTO struct {
	ID        int        `json:"id"`
	Sheet     int        `json:"sheet"` // 0 = 图框外（或无图框）
	Type      string     `json:"type"`  // notes | table | legend | title_block | text
	Title     string     `json:"title,omitempty"`
	Lines     []string   `json:"lines"` // 阅读顺序；表格/图例的单元格以 " | " 分隔
	Table     *tableDTO  `json:"table,omitempty"`
	Box       [4]float64 `json:"box"`
	TextCount int        `json:"textCount"`
}

type tableDTO struct {
	Title  string     `json:"title,omitempty"`
	Header []string   `json:"header,omitempty"`
	Rows   [][]string `json:"rows"`
}

type blockDTO struct {
	Name          string         `json:"name"`
	Label         string         `json:"label,omitempty"` // 图例识别出的中文名
	Kind          string         `json:"kind"`            // symbol | content | frame | title_block
	Count         int            `json:"count"`           // 顶层实例数（不含图例里的示意符号）
	LegendCount   int            `json:"legendCount,omitempty"`
	NestedCount   int            `json:"nestedCount,omitempty"` // 作为其他块的子块出现的次数
	SheetCounts   map[int]int    `json:"sheetCounts"`
	AttributeTags []string       `json:"attributeTags,omitempty"`
	LayerCounts   map[string]int `json:"layerCounts,omitempty"`
}

type insertDTO struct {
	ID         int               `json:"id"`
	Block      string            `json:"block"`
	Label      string            `json:"label,omitempty"`
	Kind       string            `json:"kind"`
	Legend     bool              `json:"legend,omitempty"`
	X          float64           `json:"x"`
	Y          float64           `json:"y"`
	Rotation   float64           `json:"rotation"`
	ScaleX     float64           `json:"scaleX"`
	ScaleY     float64           `json:"scaleY"`
	Layer      string            `json:"layer"`
	Sheet      int               `json:"sheet"`
	Attributes map[string]string `json:"attributes"`
}

type dimDTO struct {
	Type     string  `json:"type"`               // linear | aligned | angular | radius | diameter | ordinate | arc
	Value    float64 `json:"value"`              // 实测值（图形单位；角度为度）
	Text     string  `json:"text"`               // 图上显示的文字（含文字替代，如"约6000(现场核实)"）
	Computed bool    `json:"computed,omitempty"` // 文件未存实测值，由定义点计算
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	Layer    string  `json:"layer"`
	Sheet    int     `json:"sheet"`
}

type layerDTO struct {
	Name     string `json:"name"`
	Color    int    `json:"color"` // ACI 颜色号
	Entities int    `json:"entities"`
	Texts    int    `json:"texts"`
	Inserts  int    `json:"inserts"`
}

type textDTO struct {
	Text      string  `json:"text"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Height    float64 `json:"height"`
	Rotation  float64 `json:"rotation"`
	Layer     string  `json:"layer"`
	Sheet     int     `json:"sheet"` // 0 = 不在任何图框内
	Role      string  `json:"role"`  // notes|table|legend|title_block|text|attribute|block_text|dimension|attdef
	Block     string  `json:"block,omitempty"`
	Tag       string  `json:"tag,omitempty"`
	TextBlock int     `json:"textBlock,omitempty"` // 所属 textBlocks[].id
}
