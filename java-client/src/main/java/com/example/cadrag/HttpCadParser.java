package com.example.cadrag;

import com.fasterxml.jackson.databind.ObjectMapper;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Duration;
import java.util.UUID;

/**
 * 通过 HTTP 调用 cad-service（推荐的生产方式）。
 * 仅依赖 JDK 11 HttpClient + Jackson；Spring 项目里换成 RestClient/WebClient 也一样。
 */
public class HttpCadParser implements CadParser {

    private static final ObjectMapper MAPPER = new ObjectMapper();

    private final String baseUrl;
    private final HttpClient client;
    private final Duration timeout;

    public HttpCadParser(String baseUrl) {
        this(baseUrl, Duration.ofMinutes(5));
    }

    public HttpCadParser(String baseUrl, Duration timeout) {
        this.baseUrl = baseUrl.endsWith("/") ? baseUrl.substring(0, baseUrl.length() - 1) : baseUrl;
        this.timeout = timeout;
        this.client = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(10)).build();
    }

    @Override
    public CadParseResult parse(Path file, boolean withImages) throws IOException, InterruptedException {
        String boundary = "----cadrag" + UUID.randomUUID().toString().replace("-", "");
        byte[] body = multipartBody(boundary, "file", file);

        HttpRequest req = HttpRequest.newBuilder()
                .uri(URI.create(baseUrl + "/v1/parse?render=" + withImages))
                .timeout(timeout)
                .header("Content-Type", "multipart/form-data; boundary=" + boundary)
                .POST(HttpRequest.BodyPublishers.ofByteArray(body))
                .build();

        HttpResponse<byte[]> resp = client.send(req, HttpResponse.BodyHandlers.ofByteArray());
        if (resp.statusCode() != 200) {
            throw new IOException("cad-service " + resp.statusCode() + ": "
                    + new String(resp.body(), StandardCharsets.UTF_8));
        }
        return MAPPER.readValue(resp.body(), CadParseResult.class);
    }

    private static byte[] multipartBody(String boundary, String field, Path file) throws IOException {
        // 文件名可能含中文：filename 用 UTF-8 原样写入（Go 的 multipart 能正确解析）
        String filename = file.getFileName().toString();
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        String head = "--" + boundary + "\r\n"
                + "Content-Disposition: form-data; name=\"" + field + "\"; filename=\""
                + filename.replace("\"", "_") + "\"\r\n"
                + "Content-Type: application/octet-stream\r\n\r\n";
        out.write(head.getBytes(StandardCharsets.UTF_8));
        out.write(Files.readAllBytes(file));
        out.write(("\r\n--" + boundary + "--\r\n").getBytes(StandardCharsets.UTF_8));
        return out.toByteArray();
    }

    @SuppressWarnings("unused")
    private static String enc(String s) {
        return URLEncoder.encode(s, StandardCharsets.UTF_8);
    }
}
