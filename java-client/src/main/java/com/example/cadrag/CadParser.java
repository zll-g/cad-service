package com.example.cadrag;

import java.io.IOException;
import java.nio.file.Path;

/** CAD 解析器抽象：HTTP 微服务 / 本地 CLI 两种实现可互换。 */
public interface CadParser {

    /**
     * @param file       .dwg / .dxf / .dxfb 文件
     * @param withImages 是否同时产出每个图框的渲染图（用于多模态，较慢）
     */
    CadParseResult parse(Path file, boolean withImages) throws IOException, InterruptedException;

    static boolean isCadFile(String filename) {
        String n = filename.toLowerCase();
        return n.endsWith(".dwg") || n.endsWith(".dxf") || n.endsWith(".dxfb");
    }
}
