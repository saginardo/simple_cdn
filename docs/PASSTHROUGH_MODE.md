# HTTP(S) 透传模式与 Range 流量排障

最后审计：2026-08-09

## 结论

普通站点默认只对已识别的静态资源后缀使用 Nginx 磁盘缓存，其他 URI 直接关闭缓存。对于要求整个主机名都稳定直通的 HTTP(S) 流媒体或通用上游代理，必须启用站点级 `passthrough` 模式，而不是继续保留 `proxy_cache` 后只补充 `Range` / `If-Range` 请求头。

该模式用于整站流量，不能保留普通模式下的静态后缀缓存；需要静态资源缓存时应关闭透传并重新发布，非静态 URI 仍会保持无缓存。

## 问题特征与根因

典型故障是直接访问边缘上的通用上游代理能正常返回 Range 响应，但经过 CDN 域名的 Nginx 路径会把范围请求变成完整响应：

- 直连 `http://203.0.113.13:${EDGE_DIAGNOSTIC_PORT}/...`：`206 Partial Content`，有 `Content-Range`。
- 经 `https://stream.example.com/...`：返回 `200 OK` 和完整文件，没有 `Content-Range`。

根因是该站点仍走 Nginx `proxy_cache`。在这个模式下，原始 `Range` / `If-Range` 不应依赖于被透明传到源站；仅添加 `proxy_force_ranges` 也不能修复这个回源语义。对本项目的需求，正确方案是禁用该站点缓存并完整透传，而不是引入 slice 或视频缓存。

## 模式语义

站点 API 和持久化字段为：

```json
{
  "passthrough": true
}
```

- 仅允许 HTTP/HTTPS 源站启用。`ws://`、`wss://`、`grpc://`、`grpcs://` 已有各自的无缓存代理分支，API 会拒绝对它们启用此开关。
- 新建站点未传该字段时默认 `false`；更新站点未传时保留当前值，避免旧客户端意外关闭透传。
- 切换开关会递增 `cache_generation`，避免日后重新启用缓存时复用透传前的对象。
- 透传站点不允许“刷新缓存”，对应 API 返回 `409 Conflict`。
- 版本化 SQLite 迁移会为旧数据库补充 `passthrough INTEGER NOT NULL DEFAULT 0`，已有站点保持原有缓存行为。

启用后，Nginx 在普通主源站和备用源站位置会：

- 不生成站点级静态缓存选择策略，并显式 `proxy_cache off`。
- 设置 `proxy_buffering off` 与 `proxy_request_buffering off`。
- 使用站点配置的 2、6、15、30 或 60 分钟读写空闲超时，默认 2 分钟。
- 显式转发 `Range $http_range` 与 `If-Range $http_if_range`。
- 保留所选 HTTP/1.1、TLS HTTP/2 或 H2C 上游连接复用、SNI/TLS 校验和主备源站故障切换。

WebSocket/SSE 不再配置路径；WebSocket Upgrade、SSE Accept/控制头和 POST 会自动进入无缓存、无响应缓冲分支。`passthrough` 的作用仍是让整站所有请求都遵循无缓存转发语义，并额外关闭请求缓冲、完整转发 Range 头。

## 启用与发布

1. 确认控制面已部署包含该功能的新版；管理台“编辑站点”表单应显示“透传模式（仅 HTTP(S)，禁用 Nginx 缓存）”。
2. 编辑目标 HTTP(S) 站点，勾选该开关并保存。
3. 点击“发布”，等待所有分配的 active 边缘节点确认目标版本。保存配置本身不会修改边缘 Nginx。
4. 在站点卡片确认缓存状态显示为“透传”；“刷新缓存”操作应不再显示。
5. 对 Range 读取执行下节验证；不要仅以页面可打开或全文件下载速度作为成功标准。

## 验证方法

对支持字节范围的稳定资源，预期是 `206`、准确的 `Content-Range`，以及与请求范围一致的下载字节数：

```bash
curl --silent --show-error --fail \
  --range 0-2097151 \
  --output /dev/null \
  --dump-header - \
  --write-out '\nRESULT http=%{http_code} bytes=%{size_download} speed=%{speed_download}\n' \
  --max-time 45 \
  'https://YOUR_DOMAIN/PATH_TO_RANGE_CAPABLE_RESOURCE'
```

占位符验收请求为：

```bash
curl --silent --show-error --fail \
  --range 0-2097151 \
  --output /dev/null \
  --dump-header - \
  --write-out '\nRESULT http=%{http_code} bytes=%{size_download} speed=%{speed_download}\n' \
  --max-time 45 \
  'https://stream.example.com/PATH_TO_RANGE_CAPABLE_RESOURCE'
```

预期结果示例：

```text
HTTP/2 206
content-length: 2097152
content-range: bytes 0-2097151/TOTAL_SIZE
RESULT http=206 bytes=2097152
```

同一请求直连 `203.0.113.13:${EDGE_DIAGNOSTIC_PORT}` 也应返回相同的 `206`、`Content-Range` 和 2 MiB 字节数，说明域名经过边缘 Nginx 后没有破坏 Range 语义。

`${EDGE_DIAGNOSTIC_PORT}` 是诊断用的上游代理入口，不是正常用户流量的验收入口；日常验证应始终以业务域名的 HTTPS 请求为准，并应限制该诊断端口的公网暴露面。

## 排障顺序

1. 查询站点配置，确认 `passthrough=1`、站点已发布，且分配节点已应用新 desired state。
2. 运行上面的 Range 命令，检查 `206`、`Content-Range` 和下载字节数。
3. 若仍是 `200` 或下载了完整文件，检查控制面是否为新版、是否点击过发布、边缘 agent 是否已确认目标版本，以及实际生成的 Nginx 是否仍含该站点的 `proxy_cache $cdn_static_cache_zone`。
4. 若返回 `206` 但速度仍慢，再分别比较业务域名、边缘 `${EDGE_DIAGNOSTIC_PORT}` 和真实源站，定位网络路径或源站吞吐；不要重新启用缓存来掩盖 Range 语义问题。

相关实现：`internal/domain/domain.go`、`internal/store/store.go`、`internal/control/server.go`、`frontend/src/features/sites/`、`internal/nginx/render.go`。
