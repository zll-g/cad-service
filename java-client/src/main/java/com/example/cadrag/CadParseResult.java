package com.example.cadrag;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * CAD 解析结果，与 cad-service /v1/parse 的 JSON 一一对应（schemaVersion=2）。
 *
 * <p>v1 字段（source/dwgVersion/hasFrames/sheets/texts/elapsedMs）含义不变；
 * CLI 模式（caddocling manifest 转换）只填 v1 字段，此时 {@link #schemaVersion} = 0，
 * {@link CadChunker} 会自动退回 v1 的切片逻辑。
 *
 * <p>sheet 序号约定：1-based；0 = 图框外内容。<b>未识别到图框</b>（hasFrames=false）时，
 * sheets 里只有一个 index=1 的“整图”，而 texts/textBlocks/inserts/dimensions 的 sheet 都是 0。
 */
@JsonIgnoreProperties(ignoreUnknown = true)
public class CadParseResult {
    /** 0 = v1 / CLI 模式；2 = 当前版本 */
    public int schemaVersion;
    public String source;
    public String dwgVersion;
    /** false = 未识别到图框，整张图作为一个单元 */
    public boolean hasFrames;
    public FileInfo file;
    public Stats stats;
    public List<Sheet> sheets = new ArrayList<>();
    /** 版面分析后的文本块：notes 说明 / table 表格 / legend 图例 / title_block 标题栏 / text 普通文字 */
    public List<TextBlock> textBlocks = new ArrayList<>();
    /** 块（设备/符号）统计，按数量降序 */
    public List<Block> blocks = new ArrayList<>();
    /** 带属性的块参照明细（设备编号、回路、图框属性等） */
    public List<Insert> inserts = new ArrayList<>();
    public List<Dimension> dimensions = new ArrayList<>();
    public List<Layer> layers = new ArrayList<>();
    /** 原始文字（v1 字段 + role/block/tag/textBlock） */
    public List<Text> texts = new ArrayList<>();
    /** 解析质量提示（未识别到图框、尺寸值为计算所得…），建议记日志 */
    public List<String> warnings = new ArrayList<>();
    public long elapsedMs;

    @JsonIgnoreProperties(ignoreUnknown = true)
    public static class FileInfo {
        public String name;
        /** dwg | dxf */
        public String format;
        public long sizeBytes;
        /** 文件内容 SHA-256，可直接当文档 ID（去重、增量更新、删除旧切片） */
        public String sha256;
        public String dwgVersion;
        /** 如 "AutoCAD 2010-2012" */
        public String cadRelease;
    }

    @JsonIgnoreProperties(ignoreUnknown = true)
    public static class Stats {
        public int entities;
        public Map<String, Integer> entityTypes = new LinkedHashMap<>();
        public int layers;
        public int blockDefs;
        public int inserts;
        public int texts;
        public int dimensions;
        public int textBlocks;
        public int tables;
        public int skippedObjects;
        public int attribFixed;
    }

    @JsonIgnoreProperties(ignoreUnknown = true)
    public static class Sheet {
        /** 1-based，与 Text.sheet 对齐 */
        public int index;
        /** go-cad 识别的图名："图号-图名" > 图号 > 标题栏最大字号文本 > 块名 > "图 N" */
        public String name;
        /** 图号，如 XF-01（v2） */
        public String number;
        /** 图名，如 地下一层火灾自动报警平面图（v2） */
        public String title;
        /** 专业：建筑/结构/给排水/暖通/电气/消防/弱电/…（v2，可能为空） */
        public String discipline;
        /** 比例，如 1:100（v2，可能为空） */
        public String scale;
        /** 标题栏键值：工程名称/图号/图名/设计/校对/审核/日期/比例/版次…（v2） */
        public Map<String, String> titleBlock = new LinkedHashMap<>();
        /** 世界坐标 {minX, minY, maxX, maxY}（CLI 模式无此字段） */
        public double[] box;
        public int textCount;
        public int dimensionCount;
        /** 本图设备/符号数量（已排除图例里的示意符号与图框） */
        public List<BlockCount> blockCounts = new ArrayList<>();
        /** 本图有文字的图层（按文字数降序） */
        public List<String> layers = new ArrayList<>();
        /** render=true 时返回 */
        public String imageFormat;
        public String imageBase64;
        public int imageWidth;
        public double passRate;
        /** CLI 模式下渲染图在本地磁盘的路径 */
        public String imagePath;
    }

    @JsonIgnoreProperties(ignoreUnknown = true)
    public static class BlockCount {
        public String block;
        /** 图例里的中文名，如 FA-SD → 感烟探测器（可能为空） */
        public String label;
        public int count;
    }

    @JsonIgnoreProperties(ignoreUnknown = true)
    public static class TextBlock {
        public int id;
        public int sheet;
        /** notes | table | legend | title_block | text */
        public String type;
        public String title;
        /** 阅读顺序；表格/图例的单元格以 " | " 分隔 */
        public List<String> lines = new ArrayList<>();
        /** type=table/legend 时有值 */
        public Table table;
        public double[] box;
        public int textCount;
    }

    @JsonIgnoreProperties(ignoreUnknown = true)
    public static class Table {
        public String title;
        /** 可能为空（没识别出表头，如图例） */
        public List<String> header = new ArrayList<>();
        public List<List<String>> rows = new ArrayList<>();
    }

    @JsonIgnoreProperties(ignoreUnknown = true)
    public static class Block {
        public String name;
        public String label;
        /** symbol | content | frame | title_block */
        public String kind;
        /** 顶层实例数（不含图例里的示意符号） */
        public int count;
        public int legendCount;
        public int nestedCount;
        public Map<Integer, Integer> sheetCounts = new LinkedHashMap<>();
        public List<String> attributeTags = new ArrayList<>();
        public Map<String, Integer> layerCounts = new LinkedHashMap<>();
    }

    @JsonIgnoreProperties(ignoreUnknown = true)
    public static class Insert {
        public int id;
        public String block;
        public String label;
        public String kind;
        public boolean legend;
        public double x;
        public double y;
        public double rotation;
        public double scaleX;
        public double scaleY;
        public String layer;
        public int sheet;
        /** 属性 TAG → 值，如 {编号: 1-01, 回路: L1} */
        public Map<String, String> attributes = new LinkedHashMap<>();
    }

    @JsonIgnoreProperties(ignoreUnknown = true)
    public static class Dimension {
        /** linear | aligned | angular | radius | diameter | ordinate | arc */
        public String type;
        public double value;
        /** 图上显示的文字（含文字替代，如 "约6000(现场核实)"） */
        public String text;
        public boolean computed;
        public double x;
        public double y;
        public String layer;
        public int sheet;
    }

    @JsonIgnoreProperties(ignoreUnknown = true)
    public static class Layer {
        public String name;
        public int color;
        public int entities;
        public int texts;
        public int inserts;
    }

    @JsonIgnoreProperties(ignoreUnknown = true)
    public static class Text {
        public String text;
        public double x;
        public double y;
        public double height;
        public double rotation;
        public String layer;
        /** 所属图框序号，0 = 不在任何图框内 */
        public int sheet;
        /** notes|table|legend|title_block|text|attribute|block_text|dimension|attdef（v2） */
        public String role;
        public String block;
        public String tag;
        /** 所属 textBlocks[].id（v2） */
        public int textBlock;
    }
}
