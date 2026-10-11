package com.example.cadrag;

import java.util.LinkedHashMap;
import java.util.Map;

/**
 * 一个可直接写入向量库的切片：text 用于 embedding，metadata 用于过滤与溯源。
 * 转 Spring AI：new Document(chunk.text, chunk.metadata)
 * 转 LangChain4j：TextSegment.from(chunk.text, Metadata.from(chunk.metadata))
 */
public class CadChunk {
    public final String text;
    public final Map<String, Object> metadata;

    public CadChunk(String text, Map<String, Object> metadata) {
        this.text = text;
        this.metadata = new LinkedHashMap<>(metadata);
    }
}
