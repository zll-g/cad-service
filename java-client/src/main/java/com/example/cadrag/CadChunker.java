package com.example.cadrag;

import com.example.cadrag.CadParseResult.Block;
import com.example.cadrag.CadParseResult.Dimension;
import com.example.cadrag.CadParseResult.Insert;
import com.example.cadrag.CadParseResult.Sheet;
import com.example.cadrag.CadParseResult.Table;
import com.example.cadrag.CadParseResult.TextBlock;

import java.util.ArrayList;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeMap;
import java.util.regex.Pattern;
import java.util.stream.Collectors;

/**
 * 把 CAD 解析结果转成 RAG 切片。
 *
 * <h3>v2（cad-service schemaVersion ≥ 2）：按“内容类型”切片</h3>
 * 图纸里能回答问题的信息集中在几类结构里，分别切片、各带完整上下文头，检索时互不干扰：
 * <pre>
 *  chunkType        每份文件/每张图   内容                                         典型问题
 *  drawing_index    1 / 文件         图纸目录 + 全套设备合计                          “这套图有哪些图纸？共多少个感烟探测器？”
 *  sheet_summary    1 / 图           图号/图名/专业/比例/标题栏(设计校对审核日期…)    “XF-01 是谁设计的？比例多少？”
 *                                    + 本图设备数量 + 尺寸标注
 *  notes            n / 图           设计说明/施工说明（编号条目）                      “探测器安装间距要求？”
 *  table            n / 图           材料表/设备表 → Markdown 表格（长表分段重复表头）    “报警总线用什么型号？多少米？”
 *  legend           n / 图           图例：符号块 → 名称/型号/安装方式                   “手动报警按钮怎么安装？”
 *  equipment        n / 图           带属性的设备明细（编号、回路…）                     “1-05 在哪个回路？L2 上有哪些设备？”
 *  text             n / 图           其余文字（房间名、系统图标注…），阅读顺序 + 去重计数   “消防控制室在哪张图？”
 * </pre>
 * 标题栏碎片不单独成片（并进 sheet_summary）；纯尺寸数字不进正文（尺寸以 dimensions 汇总）。
 *
 * <h3>v1 回退</h3>
 * schemaVersion=0（CLI 模式 / 老服务）时按 v1 逻辑：按图框分组、阅读顺序、去重计数、按长度切分。
 *
 * <h3>元数据</h3>
 * 所有值都是 String/Number（各家向量库都支持过滤）；为空的键不写入。
 * docId = 文件 SHA-256，chunkId = docId 前 16 位 + 图序号 + 类型 + 序号（稳定，可用于 upsert）。
 */
public class CadChunker {

    /** 每个切片正文的最大字符数（不含上下文头），中文 embedding 模型一般 500~1000 合适 */
    private int maxChars = 800;
    /** 相邻切片的重叠行数（notes/text 类型；表格与设备明细改为重复表头） */
    private int overlapLines = 2;
    /** 是否丢弃纯尺寸/数字类文本 */
    private boolean dropDimensionTexts = true;
    /** sheet_summary 里最多列出多少个不同的尺寸标注文字 */
    private int maxDimensionsInSummary = 30;

    // 纯数字 / 尺寸标注：数字、小数点/逗号、φ、±、°、×、R、%、括号、英制 ' " 等组合
    private static final Pattern DIMENSION = Pattern.compile(
            "^[\\s\\d.,，:：±+\\-−×xX*/%°φΦØ∅RrMm()（）\\[\\]=~<>@#'\"′″]+$");
    private static final Pattern WS = Pattern.compile("[ \\t\\u3000]+");
    /** 标题栏里优先展示的键（其余按原顺序附在后面） */
    private static final List<String> TITLE_KEY_ORDER = List.of(
            "工程名称", "子项名称", "图号", "图名", "专业", "比例", "阶段", "版次", "日期",
            "设计", "制图", "校对", "审核", "审定", "专业负责人", "项目负责人", "设计单位", "建设单位");

    public CadChunker maxChars(int v) { this.maxChars = v; return this; }
    public CadChunker overlapLines(int v) { this.overlapLines = v; return this; }
    public CadChunker dropDimensionTexts(boolean v) { this.dropDimensionTexts = v; return this; }
    public CadChunker maxDimensionsInSummary(int v) { this.maxDimensionsInSummary = v; return this; }

    public List<CadChunk> chunk(CadParseResult r) {
        if (r.schemaVersion >= 2) return chunkV2(r);
        return chunkLegacy(r);
    }

    // =====================================================================
    // v2
    // =====================================================================

    /** 一张“逻辑图纸”的上下文：真实图框，或图框外内容（index 0），或无图框时的整图 */
    private static final class SheetCtx {
        int index;
        Sheet sheet;               // 图框外内容时为 null
        String displayName;        // 上下文头里用：XF-01 地下一层火灾自动报警平面图
        Map<String, Object> baseMd;
        String header;
    }

    private List<CadChunk> chunkV2(CadParseResult r) {
        List<CadChunk> out = new ArrayList<>();
        String docId = r.file != null && r.file.sha256 != null ? r.file.sha256 : null;
        String project = firstTitleValue(r, "工程名称");

        // ---- 逻辑图纸：真实图框 + 图框外（index 0）；无图框时 index 0 的内容归到整图 ----
        Map<Integer, SheetCtx> ctxs = new TreeMap<>();
        for (Sheet s : r.sheets) ctxs.put(s.index, newCtx(r, s, s.index, docId, project));
        if (r.hasFrames) ctxs.put(0, newCtx(r, null, 0, docId, project));
        java.util.function.IntFunction<SheetCtx> ctxOf = sheetNo -> {
            if (!r.hasFrames) return ctxs.values().iterator().next();
            SheetCtx c = ctxs.get(sheetNo);
            return c != null ? c : ctxs.get(0);
        };
        if (ctxs.isEmpty()) {       // 理论上不会发生：服务端至少返回一个“整图”
            Sheet s = new Sheet();
            s.index = 1;
            s.name = "整图";
            ctxs.put(1, newCtx(r, s, 1, docId, project));
        }

        // ---- 1. drawing_index ----
        out.add(drawingIndex(r, docId, project));

        // ---- 2. sheet_summary（每张真实图纸一个） ----
        Map<Integer, List<TextBlock>> blocksBySheet = new TreeMap<>();
        for (TextBlock b : r.textBlocks) blocksBySheet.computeIfAbsent(ctxOf.apply(b.sheet).index, k -> new ArrayList<>()).add(b);
        Map<Integer, List<Dimension>> dimsBySheet = new TreeMap<>();
        for (Dimension d : r.dimensions) dimsBySheet.computeIfAbsent(ctxOf.apply(d.sheet).index, k -> new ArrayList<>()).add(d);

        for (SheetCtx c : ctxs.values()) {
            if (c.sheet == null) continue;
            sheetSummary(out, c, blocksBySheet.getOrDefault(c.index, List.of()),
                    dimsBySheet.getOrDefault(c.index, List.of()));
        }

        // ---- 3~5. notes / table / legend：一个文本块一组 ----
        for (Map.Entry<Integer, List<TextBlock>> e : blocksBySheet.entrySet()) {
            SheetCtx c = ctxs.get(e.getKey());
            for (TextBlock b : e.getValue()) {
                switch (b.type == null ? "text" : b.type) {
                    case "notes":
                        emit(out, c, "notes", b.title, notesLines(b), List.of(), overlapLines,
                                Map.of("textBlockId", b.id));
                        break;
                    case "table":
                    case "legend":
                        emitTable(out, c, b);
                        break;
                    default:
                        break; // title_block → summary；text → 下面合并
                }
            }
        }

        // ---- 6. equipment：带属性的设备明细，按图纸、块分组 ----
        Map<Integer, Map<String, List<Insert>>> eq = new TreeMap<>();
        for (Insert in : r.inserts) {
            if (in.legend || in.attributes == null || in.attributes.isEmpty()) continue;
            if ("frame".equals(in.kind) || "title_block".equals(in.kind)) continue;
            eq.computeIfAbsent(ctxOf.apply(in.sheet).index, k -> new TreeMap<>())
                    .computeIfAbsent(in.block, k -> new ArrayList<>()).add(in);
        }
        for (Map.Entry<Integer, Map<String, List<Insert>>> e : eq.entrySet()) {
            SheetCtx c = ctxs.get(e.getKey());
            List<Map.Entry<String, List<Insert>>> groups = new ArrayList<>(e.getValue().entrySet());
            groups.sort((a, b) -> b.getValue().size() - a.getValue().size()); // 数量多的设备在前
            for (Map.Entry<String, List<Insert>> be : groups) {
                List<Insert> list = be.getValue();
                String label = list.get(0).label;
                String name = label != null && !label.isEmpty() ? label + "（" + be.getKey() + "）" : be.getKey();
                List<String> head = List.of("设备明细：" + name + "，共 " + list.size() + " 个");
                List<String> lines = new ArrayList<>();
                for (Insert in : list) {
                    lines.add("- " + in.attributes.entrySet().stream()
                            .filter(a -> a.getValue() != null && !a.getValue().isBlank())
                            .sorted(Comparator.comparingInt(a -> idTagRank(a.getKey())))
                            .map(a -> a.getKey() + "=" + a.getValue())
                            .collect(Collectors.joining("，")));
                }
                Map<String, Object> extra = new LinkedHashMap<>();
                extra.put("block", be.getKey());
                if (label != null && !label.isEmpty()) extra.put("blockLabel", label);
                emit(out, c, "equipment", name, lines, head, 0, extra);
            }
        }

        // ---- 7. text：其余文字（每张图合并为一组，按阅读顺序） ----
        for (Map.Entry<Integer, List<TextBlock>> e : blocksBySheet.entrySet()) {
            SheetCtx c = ctxs.get(e.getKey());
            List<TextBlock> others = e.getValue().stream()
                    .filter(b -> b.type == null || "text".equals(b.type))
                    .sorted(Comparator.comparingDouble((TextBlock b) -> b.box == null ? 0 : -b.box[3])
                            .thenComparingDouble(b -> b.box == null ? 0 : b.box[0]))
                    .collect(Collectors.toList());
            List<String> lines = otherTextLines(others);
            if (!lines.isEmpty()) emit(out, c, "text", "图面文字", lines, List.of(), overlapLines, Map.of());
        }

        // chunkId / chunkIndex / chunkTotal（按 sheet + chunkType 分组编号）
        Map<String, Integer> totals = new LinkedHashMap<>();
        for (CadChunk ch : out) totals.merge(groupKey(ch), 1, Integer::sum);
        Map<String, Integer> seen = new LinkedHashMap<>();
        String idPrefix = docId != null ? docId.substring(0, Math.min(16, docId.length())) : safeId(r.source);
        for (CadChunk ch : out) {
            String g = groupKey(ch);
            int i = seen.merge(g, 1, Integer::sum) - 1;
            ch.metadata.put("chunkIndex", i);
            ch.metadata.put("chunkTotal", totals.get(g));
            ch.metadata.put("chunkId", idPrefix + "-s" + ch.metadata.get("sheetIndex") + "-" + ch.metadata.get("chunkType") + "-" + i);
        }
        return out;
    }

    private SheetCtx newCtx(CadParseResult r, Sheet s, int index, String docId, String project) {
        SheetCtx c = new SheetCtx();
        c.index = index;
        c.sheet = s;
        Map<String, Object> md = new LinkedHashMap<>();
        md.put("docType", "cad");
        md.put("source", r.source);
        put(md, "docId", docId);
        put(md, "dwgVersion", r.dwgVersion);
        put(md, "project", project);
        md.put("sheetIndex", index);
        if (s == null) {
            c.displayName = "图框外内容";
        } else {
            String num = nz(s.number), title = nz(s.title);
            String disp = (num + " " + title).trim();
            if (disp.isEmpty()) disp = nz(s.name).isEmpty() ? "图 " + index : s.name;
            c.displayName = disp;
            put(md, "sheetNumber", num);
            put(md, "sheetTitle", title);
            put(md, "discipline", s.discipline);
            put(md, "scale", s.scale);
            Map<String, String> tb = s.titleBlock == null ? Map.of() : s.titleBlock;
            put(md, "designer", tb.get("设计"));
            put(md, "checker", tb.get("校对"));
            put(md, "reviewer", tb.get("审核"));
            put(md, "date", tb.get("日期"));
            put(md, "revision", tb.get("版次"));
        }
        md.put("sheetName", c.displayName);
        c.baseMd = md;

        StringBuilder h = new StringBuilder("【CAD图纸】文件：").append(r.source);
        if (project != null) h.append("；工程：").append(project);
        h.append("；图纸：").append(c.displayName);
        if (s != null) {
            List<String> extra = new ArrayList<>();
            if (!nz(s.discipline).isEmpty()) extra.add(s.discipline);
            if (!nz(s.scale).isEmpty()) extra.add(s.scale);
            if (!extra.isEmpty()) h.append("（").append(String.join("，", extra)).append("）");
        }
        c.header = h.toString();
        return c;
    }

    private CadChunk drawingIndex(CadParseResult r, String docId, String project) {
        List<String> lines = new ArrayList<>();
        lines.add("图纸目录（共 " + (r.hasFrames ? r.sheets.size() + " 张" : "1 张，未识别到图框") + "）：");
        for (Sheet s : r.sheets) {
            String disp = (nz(s.number) + " " + nz(s.title)).trim();
            if (disp.isEmpty()) disp = nz(s.name);
            List<String> extra = new ArrayList<>();
            if (!nz(s.discipline).isEmpty()) extra.add(s.discipline);
            if (!nz(s.scale).isEmpty()) extra.add("比例 " + s.scale);
            lines.add(s.index + ". " + disp + (extra.isEmpty() ? "" : "（" + String.join("，", extra) + "）"));
        }
        List<String> eq = new ArrayList<>();
        for (Block b : r.blocks) {
            if (!"symbol".equals(b.kind) || b.count <= 0) continue;
            eq.add(blockName(b.label, b.name) + " " + b.count + " 个");
        }
        if (!eq.isEmpty()) lines.add("全套设备/符号数量（不含图例示意）：" + String.join("；", eq));
        Map<String, Object> md = new LinkedHashMap<>();
        md.put("docType", "cad");
        md.put("source", r.source);
        put(md, "docId", docId);
        put(md, "dwgVersion", r.dwgVersion);
        put(md, "project", project);
        md.put("sheetIndex", -1);
        md.put("sheetName", "全部图纸");
        md.put("chunkType", "drawing_index");
        md.put("sheetCount", r.sheets.size());
        if (r.file != null) put(md, "cadRelease", r.file.cadRelease);
        String header = "【CAD图纸】文件：" + r.source + (project != null ? "；工程：" + project : "") + "；内容：图纸目录";
        return new CadChunk(header + "\n" + String.join("\n", lines), md);
    }

    private void sheetSummary(List<CadChunk> out, SheetCtx c, List<TextBlock> blocks, List<Dimension> dims) {
        Sheet s = c.sheet;
        List<String> lines = new ArrayList<>();
        lines.add("图纸信息：" + c.displayName);
        // 标题栏：优先结构化键值；没有键值时退回标题栏文本块原文
        Map<String, String> tb = s.titleBlock == null ? Map.of() : s.titleBlock;
        Map<String, String> merged = new LinkedHashMap<>(tb);
        if (!nz(s.number).isEmpty()) merged.putIfAbsent("图号", s.number);
        if (!nz(s.title).isEmpty()) merged.putIfAbsent("图名", s.title);
        if (!nz(s.discipline).isEmpty()) merged.putIfAbsent("专业", s.discipline);
        if (!nz(s.scale).isEmpty()) merged.putIfAbsent("比例", s.scale);
        List<String> kv = new ArrayList<>();
        for (String k : TITLE_KEY_ORDER) {
            if (merged.containsKey(k)) kv.add(k + "：" + merged.get(k));
        }
        for (Map.Entry<String, String> e : merged.entrySet()) {
            if (!TITLE_KEY_ORDER.contains(e.getKey())) kv.add(e.getKey() + "：" + e.getValue());
        }
        if (tb.isEmpty()) {
            for (TextBlock b : blocks) {
                if ("title_block".equals(b.type)) for (String l : b.lines) kv.add(clean(l));
            }
        }
        if (!kv.isEmpty()) lines.add("标题栏：" + String.join("；", kv));

        if (s.blockCounts != null && !s.blockCounts.isEmpty()) {
            lines.add("本图设备/符号数量：" + s.blockCounts.stream()
                    .map(b -> blockName(b.label, b.block) + " " + b.count + " 个")
                    .collect(Collectors.joining("；")));
        }
        // 本图内容概览：各类文本块标题，方便“这张图里有没有材料表”这类问题
        List<String> parts = new ArrayList<>();
        for (TextBlock b : blocks) {
            if (("notes".equals(b.type) || "table".equals(b.type) || "legend".equals(b.type)) && !nz(b.title).isEmpty()) {
                parts.add(b.title);
            }
        }
        if (!parts.isEmpty()) lines.add("本图包含：" + String.join("、", parts));
        if (!dims.isEmpty()) {
            Map<String, Integer> freq = new LinkedHashMap<>();
            for (Dimension d : dims) {
                String t = nz(d.text).isEmpty() ? fmt(d.value) : clean(d.text);
                if (!t.isEmpty()) freq.merge(t, 1, Integer::sum);
            }
            String list = freq.entrySet().stream().limit(maxDimensionsInSummary)
                    .map(e -> e.getValue() > 1 ? e.getKey() + "(×" + e.getValue() + ")" : e.getKey())
                    .collect(Collectors.joining("、"));
            lines.add("尺寸标注 " + dims.size() + " 个：" + list + (freq.size() > maxDimensionsInSummary ? " …" : ""));
        }
        if (s.layers != null && !s.layers.isEmpty()) {
            lines.add("图层：" + s.layers.stream().limit(20).collect(Collectors.joining("、")));
        }
        Map<String, Object> md = new LinkedHashMap<>(c.baseMd);
        md.put("chunkType", "sheet_summary");
        md.put("textCount", s.textCount);
        md.put("dimensionCount", dims.size());
        if (s.blockCounts != null && !s.blockCounts.isEmpty()) {
            md.put("blocks", s.blockCounts.stream().map(b -> b.block).limit(30).collect(Collectors.joining(",")));
        }
        if (s.imagePath != null) md.put("imagePath", s.imagePath);
        // 摘要通常很短；极端情况下（几百种设备、上百个尺寸）按长度切，每段都重复“图纸信息”行
        if (lines.size() == 1) {
            out.add(new CadChunk(c.header + "\n" + lines.get(0), md));
            return;
        }
        for (String body : split(lines.subList(1, lines.size()), 0, lines.subList(0, 1))) {
            out.add(new CadChunk(c.header + "\n" + body, md));
        }
    }

    private List<String> notesLines(TextBlock b) {
        List<String> lines = new ArrayList<>();
        for (String l : b.lines) {
            String s = clean(l);
            if (!s.isEmpty()) lines.add(s);
        }
        return lines;
    }

    private void emitTable(List<CadChunk> out, SheetCtx c, TextBlock b) {
        boolean legend = "legend".equals(b.type);
        Table t = b.table;
        String title = !nz(b.title).isEmpty() ? b.title : (legend ? "图例" : "表格");
        if (t == null || t.rows == null || t.rows.isEmpty()) {   // 没还原出表格结构：按普通行处理
            emit(out, c, b.type, title, notesLines(b), List.of(), 0, Map.of("textBlockId", b.id));
            return;
        }
        int cols = t.header == null ? 0 : t.header.size();
        for (List<String> row : t.rows) cols = Math.max(cols, row.size());
        List<String> header = new ArrayList<>();
        if (t.header != null && !t.header.isEmpty()) {
            header.addAll(t.header);
        } else if (legend) {
            List<String> names = List.of("图例符号", "名称", "型号规格", "说明");
            for (int i = 0; i < cols; i++) header.add(i < names.size() ? names.get(i) : "列" + (i + 1));
        } else {
            for (int i = 0; i < cols; i++) header.add("列" + (i + 1));
        }
        while (header.size() < cols) header.add("列" + (header.size() + 1));

        List<String> head = new ArrayList<>();
        head.add(legend ? ("图例".equals(title) ? "图例" : "图例：" + title) : "表格：" + title);
        head.add(mdRow(header, cols));
        head.add("|" + " --- |".repeat(cols));
        List<String> rows = new ArrayList<>();
        for (List<String> row : t.rows) rows.add(mdRow(row, cols));
        Map<String, Object> extra = new LinkedHashMap<>();
        extra.put("textBlockId", b.id);
        extra.put("tableTitle", title);
        extra.put("tableColumns", String.join(",", header));
        extra.put("rowCount", t.rows.size());
        emit(out, c, b.type, title, rows, head, 0, extra);
    }

    private static String mdRow(List<String> cells, int cols) {
        StringBuilder sb = new StringBuilder("|");
        for (int i = 0; i < cols; i++) {
            String v = i < cells.size() ? clean(cells.get(i)) : "";
            sb.append(' ').append(v.replace("|", "\\|")).append(" |");
        }
        return sb.toString();
    }

    /** 普通文字：每个文本块的行按阅读顺序拼接，跨块去重并计数，过滤纯尺寸数字 */
    private List<String> otherTextLines(List<TextBlock> blocks) {
        Map<String, Integer> freq = new LinkedHashMap<>();
        List<String> ordered = new ArrayList<>();
        for (TextBlock b : blocks) {
            for (String l : b.lines) {
                String s = clean(l);
                if (s.isEmpty() || (dropDimensionTexts && DIMENSION.matcher(s).matches())) continue;
                if (freq.merge(s, 1, Integer::sum) == 1) ordered.add(s);
            }
        }
        List<String> lines = new ArrayList<>();
        for (String s : ordered) {
            int n = freq.get(s);
            lines.add(n > 1 ? s + "(×" + n + ")" : s);
        }
        return lines;
    }

    /**
     * 把一组行切成若干切片并加入 out。
     * head：每个切片都重复的头部行（表头、设备名）；overlap：相邻切片重叠行数。
     */
    private void emit(List<CadChunk> out, SheetCtx c, String type, String title, List<String> lines,
                      List<String> head, int overlap, Map<String, Object> extra) {
        if (lines.isEmpty()) return;
        List<String> bodies = split(lines, overlap, head);
        String header = c.header + (title != null && !title.isEmpty() ? "；内容：" + title : "");
        for (String body : bodies) {
            Map<String, Object> md = new LinkedHashMap<>(c.baseMd);
            md.put("chunkType", type);
            if (title != null && !title.isEmpty()) md.put("title", title);
            md.putAll(extra);
            out.add(new CadChunk(header + "\n" + body, md));
        }
    }

    private static final Pattern ID_TAG = Pattern.compile(
            "(编号|位号|标号|地址|^NO\\.?$|^ID$|TAG|^NUM|NUMBER|^NAME$|名称)", Pattern.CASE_INSENSITIVE);

    /** 编号/位号类属性排前面（服务端 JSON 的属性键是按字典序的，“回路”会排在“编号”前） */
    private static int idTagRank(String tag) {
        return ID_TAG.matcher(tag).find() ? 0 : 1;
    }

    private static String groupKey(CadChunk ch) {
        return ch.metadata.get("sheetIndex") + "/" + ch.metadata.get("chunkType");
    }

    private static String firstTitleValue(CadParseResult r, String key) {
        for (Sheet s : r.sheets) {
            if (s.titleBlock != null && !nz(s.titleBlock.get(key)).isEmpty()) return s.titleBlock.get(key);
        }
        return null;
    }

    private static String blockName(String label, String block) {
        return label != null && !label.isEmpty() ? label + "(" + block + ")" : block;
    }

    private static void put(Map<String, Object> md, String k, String v) {
        if (v != null && !v.isBlank()) md.put(k, v);
    }

    private static String nz(String s) { return s == null ? "" : s.trim(); }

    private static String fmt(double v) {
        if (Math.abs(v - Math.rint(v)) < 1e-6) return String.valueOf((long) Math.rint(v));
        return String.format("%.2f", v);
    }

    private static String safeId(String s) {
        return s == null ? "cad" : s.replaceAll("[^0-9A-Za-z_.-]", "_");
    }

    // =====================================================================
    // v1（CLI 模式 / 老服务）
    // =====================================================================

    /**
     * CAD 文本与普通文档不同：没有段落顺序，散落在二维坐标上，且大量是尺寸数字、
     * 重复的块标注。v1 只有原始文字时的做法：
     *  1. 按图框分组；2. 按阅读顺序重排；3. 去重计数；4. 过滤纯尺寸数字；
     *  5. 每个切片带上下文头；6. 超长的图框按行切分，带少量行重叠。
     */
    private List<CadChunk> chunkLegacy(CadParseResult r) {
        List<CadChunk> out = new ArrayList<>();

        Map<Integer, List<CadParseResult.Text>> bySheet = new LinkedHashMap<>();
        Map<Integer, Sheet> sheetMeta = new LinkedHashMap<>();
        if (r.hasFrames) {
            for (Sheet s : r.sheets) {
                bySheet.put(s.index, new ArrayList<>());
                sheetMeta.put(s.index, s);
            }
            bySheet.put(0, new ArrayList<>()); // 图框外
            for (CadParseResult.Text t : r.texts) {
                bySheet.computeIfAbsent(t.sheet, k -> new ArrayList<>()).add(t);
            }
        } else {
            bySheet.put(1, new ArrayList<>(r.texts));
            if (!r.sheets.isEmpty()) sheetMeta.put(1, r.sheets.get(0));
        }

        for (Map.Entry<Integer, List<CadParseResult.Text>> e : bySheet.entrySet()) {
            int idx = e.getKey();
            List<CadParseResult.Text> texts = e.getValue();
            if (texts.isEmpty()) continue;

            Sheet sheet = sheetMeta.get(idx);
            String sheetName = idx == 0 ? "图框外内容"
                    : (sheet != null && sheet.name != null ? sheet.name : "图 " + idx);

            List<CadParseResult.Text> kept = new ArrayList<>();
            int dimensionCount = 0;
            Set<String> layers = new LinkedHashSet<>();
            for (CadParseResult.Text t : texts) {
                String s = clean(t.text);
                if (s.isEmpty()) continue;
                if (dropDimensionTexts && DIMENSION.matcher(s).matches()) {
                    dimensionCount++;
                    continue;
                }
                t.text = s;
                kept.add(t);
                if (t.layer != null && !t.layer.isEmpty()) layers.add(t.layer);
            }
            List<String> lines = toReadingLines(kept);
            if (lines.isEmpty()) continue;

            String header = "【CAD图纸】文件：" + r.source + "；图纸：" + sheetName
                    + (idx > 0 && r.hasFrames ? "（第" + idx + "张）" : "") + "\n";
            List<String> bodies = split(lines, overlapLines, List.of());
            for (int i = 0; i < bodies.size(); i++) {
                Map<String, Object> md = new LinkedHashMap<>();
                md.put("docType", "cad");
                md.put("source", r.source);
                md.put("dwgVersion", r.dwgVersion);
                md.put("sheetIndex", idx);
                md.put("sheetName", sheetName);
                md.put("chunkType", "sheet_text");
                md.put("chunkIndex", i);
                md.put("chunkTotal", bodies.size());
                md.put("textCount", kept.size());
                md.put("dimensionCount", dimensionCount);
                md.put("layers", layers.stream().limit(30).collect(Collectors.joining(",")));
                if (sheet != null && sheet.imagePath != null) md.put("imagePath", sheet.imagePath);
                out.add(new CadChunk(header + bodies.get(i), md));
            }
        }
        return out;
    }

    /** 去掉多余空白；MTEXT 多行用空格合并（行内语义在同一处，不应拆开） */
    private static String clean(String s) {
        if (s == null) return "";
        s = s.replace("\r", "").replace('\n', ' ');
        return WS.matcher(s).replaceAll(" ").trim();
    }

    /**
     * 阅读顺序：按 Y 降序（CAD 的 Y 轴向上），Y 差在字高 0.6 倍内视为同一行，
     * 行内按 X 升序。重复文本只在首次出现处输出，并追加 (×N)。
     */
    private static List<String> toReadingLines(List<CadParseResult.Text> texts) {
        Map<String, Integer> freq = new LinkedHashMap<>();
        for (CadParseResult.Text t : texts) freq.merge(t.text, 1, Integer::sum);

        List<CadParseResult.Text> sorted = new ArrayList<>(texts);
        sorted.sort(Comparator.comparingDouble((CadParseResult.Text t) -> -t.y)
                .thenComparingDouble(t -> t.x));

        List<List<CadParseResult.Text>> rows = new ArrayList<>();
        List<CadParseResult.Text> cur = new ArrayList<>();
        double rowY = Double.NaN, rowH = 0;
        for (CadParseResult.Text t : sorted) {
            double h = t.height > 0 ? t.height : 1;
            if (!cur.isEmpty() && Math.abs(rowY - t.y) > Math.max(rowH, h) * 0.6) {
                rows.add(cur);
                cur = new ArrayList<>();
            }
            if (cur.isEmpty()) { rowY = t.y; rowH = h; }
            cur.add(t);
        }
        if (!cur.isEmpty()) rows.add(cur);

        Set<String> emitted = new LinkedHashSet<>();
        List<String> lines = new ArrayList<>();
        for (List<CadParseResult.Text> row : rows) {
            row.sort(Comparator.comparingDouble(t -> t.x));
            List<String> cells = new ArrayList<>();
            for (CadParseResult.Text t : row) {
                if (!emitted.add(t.text)) continue;
                int n = freq.get(t.text);
                cells.add(n > 1 ? t.text + "(×" + n + ")" : t.text);
            }
            if (!cells.isEmpty()) lines.add(String.join("  ", cells));
        }
        return lines;
    }

    /**
     * 按行累积到 maxChars 切分：相邻切片重叠 overlap 行；head 行在每个切片开头重复
     * （表头 / 设备名，不计入重叠）；超长单行硬切。
     */
    private List<String> split(List<String> lines, int overlap, List<String> head) {
        int headLen = head.stream().mapToInt(h -> h.length() + 1).sum();
        int budget = Math.max(100, maxChars - headLen);
        List<String> expanded = new ArrayList<>();
        for (String l : lines) {
            while (l.length() > budget) {
                expanded.add(l.substring(0, budget));
                l = l.substring(budget);
            }
            expanded.add(l);
        }
        String prefix = head.isEmpty() ? "" : String.join("\n", head) + "\n";
        List<String> chunks = new ArrayList<>();
        int start = 0;
        while (start < expanded.size()) {
            int len = 0, end = start;
            while (end < expanded.size() && (end == start || len + expanded.get(end).length() + 1 <= budget)) {
                len += expanded.get(end).length() + 1;
                end++;
            }
            chunks.add(prefix + String.join("\n", expanded.subList(start, end)));
            if (end >= expanded.size()) break;
            start = Math.max(start + 1, end - overlap);
        }
        return chunks;
    }
}
