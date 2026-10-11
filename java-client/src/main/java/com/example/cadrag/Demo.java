package com.example.cadrag;

import com.fasterxml.jackson.databind.ObjectMapper;

import java.io.BufferedWriter;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.time.Duration;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;

/**
 * 用法：
 *   java Demo http http://localhost:8090 out.jsonl a.dwg b.dxf ...
 *   java Demo cli  /path/to/caddocling   out.jsonl a.dwg b.dxf ...
 *
 * 输出 JSONL：每行一个切片 {"id":..., "text":..., "metadata":{...}}，可直接导入向量库。
 * 环境变量 CAD_DEMO_PREVIEW=all 打印全部切片（默认每种类型预览一个）。
 */
public class Demo {
    public static void main(String[] args) throws Exception {
        if (args.length < 4) {
            System.err.println("usage: Demo <http|cli> <serviceUrl|caddoclingPath> <out.jsonl> <files...>");
            System.exit(2);
        }
        CadParser parser = "cli".equals(args[0])
                ? new CliCadParser(args[1], Paths.get("cad-work"), 300)
                : new HttpCadParser(args[1], Duration.ofMinutes(5));
        CadChunker chunker = new CadChunker().maxChars(800).overlapLines(2);
        ObjectMapper om = new ObjectMapper();
        boolean previewAll = "all".equalsIgnoreCase(System.getenv("CAD_DEMO_PREVIEW"));

        try (BufferedWriter w = Files.newBufferedWriter(Paths.get(args[2]), StandardCharsets.UTF_8)) {
            for (int i = 3; i < args.length; i++) {
                Path f = Paths.get(args[i]);
                long t0 = System.currentTimeMillis();
                CadParseResult r;
                try {
                    r = parser.parse(f, false);
                } catch (Exception ex) {
                    System.out.printf("%s  解析失败：%s%n", f.getFileName(), ex.getMessage());
                    continue;
                }
                List<CadChunk> chunks = chunker.chunk(r);

                Map<String, Integer> byType = new TreeMap<>();
                for (CadChunk c : chunks) byType.merge(String.valueOf(c.metadata.get("chunkType")), 1, Integer::sum);
                System.out.printf("%s  schema=v%d  版本=%s  图框=%s  文本=%d  文本块=%d  设备种类=%d  切片=%d %s  耗时=%dms%n",
                        f.getFileName(), r.schemaVersion, r.dwgVersion,
                        r.hasFrames ? r.sheets.size() : "无(整图)", r.texts.size(), r.textBlocks.size(),
                        r.blocks.size(), chunks.size(), byType, System.currentTimeMillis() - t0);
                for (String warn : r.warnings) System.out.println("  ⚠ " + warn);

                java.util.Set<String> previewed = new java.util.HashSet<>();
                for (CadChunk c : chunks) {
                    Object id = c.metadata.getOrDefault("chunkId", f.getFileName() + "-" + previewed.size());
                    w.write(om.writeValueAsString(Map.of("id", id, "text", c.text, "metadata", c.metadata)));
                    w.newLine();
                    String type = String.valueOf(c.metadata.get("chunkType"));
                    if (previewAll || previewed.add(type)) {
                        System.out.println("  ── [" + type + "] " + id + " ──");
                        System.out.println("  " + c.text.replace("\n", "\n  "));
                    }
                }
                if (!chunks.isEmpty()) System.out.println("  metadata 示例=" + chunks.get(chunks.size() > 1 ? 1 : 0).metadata);
            }
        }
    }
}
