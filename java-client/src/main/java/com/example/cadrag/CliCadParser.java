package com.example.cadrag;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;

/**
 * 直接调用 go-cad 自带的 caddocling 命令行（无需额外部署服务）。
 *
 * 注意：caddocling 每次都会渲染拆图（为保证文字清晰可能自动放大重渲），
 * 大图纸耗时明显高于 HTTP 服务的纯文本模式，适合小批量 / 离线场景。
 *
 * 部署提示：拆图文件名包含中文图名，JVM 所在环境请设置 LANG=C.UTF-8（或 zh_CN.UTF-8），
 * 否则 Java 的 Path API 无法表示这些文件名。
 */
public class CliCadParser implements CadParser {

    private static final ObjectMapper MAPPER = new ObjectMapper();

    private final String binary;       // caddocling 可执行文件路径
    private final long timeoutSeconds;
    private final Path workRoot;       // 输出根目录（渲染图会保留在这里，供多模态使用）

    public CliCadParser(String binary, Path workRoot, long timeoutSeconds) {
        this.binary = binary;
        this.workRoot = workRoot;
        this.timeoutSeconds = timeoutSeconds;
    }

    @Override
    public CadParseResult parse(Path file, boolean withImages) throws IOException, InterruptedException {
        Files.createDirectories(workRoot);
        Path outDir = Files.createTempDirectory(workRoot, "cad-");

        List<String> cmd = new ArrayList<>();
        cmd.add(binary);
        cmd.add(file.toAbsolutePath().toString());
        cmd.add("-o");
        cmd.add(outDir.toString());
        cmd.add("--no-full-image");

        Process p = new ProcessBuilder(cmd)
                .redirectErrorStream(true)
                .redirectOutput(outDir.resolve("caddocling.log").toFile())
                .start();
        if (!p.waitFor(timeoutSeconds, TimeUnit.SECONDS)) {
            p.destroyForcibly();
            throw new IOException("caddocling timeout after " + timeoutSeconds + "s: " + file);
        }
        if (p.exitValue() != 0) {
            String log = Files.readString(outDir.resolve("caddocling.log"), StandardCharsets.UTF_8);
            throw new IOException("caddocling exit " + p.exitValue() + ": " + log);
        }

        CadParseResult r = fromManifest(MAPPER.readTree(outDir.resolve("manifest.json").toFile()), outDir);
        if (!withImages) {
            deleteImages(outDir);
            for (CadParseResult.Sheet s : r.sheets) s.imagePath = null;
        }
        return r;
    }

    /** manifest.json（schema_version=1，snake_case）→ 统一模型 */
    static CadParseResult fromManifest(JsonNode m, Path outDir) {
        CadParseResult r = new CadParseResult();
        r.source = m.path("source").asText();
        r.dwgVersion = m.path("dwg_version").asText();
        for (JsonNode s : m.path("sheets")) {
            CadParseResult.Sheet sh = new CadParseResult.Sheet();
            sh.index = s.path("index").asInt();
            sh.name = s.path("name").asText();
            sh.textCount = s.path("text_count").asInt();
            sh.imageWidth = s.path("width").asInt();
            sh.passRate = s.path("pass_rate").asDouble();
            // 拆图文件名含中文图名：用字符串拼接而不是 Path.resolve，
            // 避免 JVM 在非 UTF-8 locale（如 Docker 默认 POSIX）下抛 InvalidPathException
            sh.imagePath = outDir.toString() + java.io.File.separator + s.path("image").asText();
            r.sheets.add(sh);
        }
        for (JsonNode t : m.path("texts")) {
            CadParseResult.Text tx = new CadParseResult.Text();
            tx.text = t.path("text").asText();
            tx.x = t.path("x").asDouble();
            tx.y = t.path("y").asDouble();
            tx.height = t.path("height").asDouble();
            tx.rotation = t.path("rotation").asDouble();
            tx.layer = t.path("layer").asText();
            tx.sheet = t.path("sheet").asInt();
            r.texts.add(tx);
        }
        r.hasFrames = !(r.sheets.size() == 1 && "整图".equals(r.sheets.get(0).name));
        return r;
    }

    private static void deleteImages(Path dir) {
        // 使用 File API 列目录：非 UTF-8 locale 下中文文件名也能删除
        java.io.File[] files = dir.toFile().listFiles();
        if (files == null) return;
        for (java.io.File f : files) {
            String n = f.getName();
            if (n.endsWith(".png") || n.endsWith(".svg")) f.delete();
        }
    }
}
