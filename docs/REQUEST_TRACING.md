# 全链路请求追踪

项目为每个 HTTP/gRPC 业务请求建立一条可检索的请求级关联链路，覆盖客户端、边缘 Nginx、HTTP/H2C/HTTPS/WebSocket/gRPC 源站、边缘日志队列、ClickHouse 和控制台。它用于定位一次请求经过哪个节点、访问哪个源站、是否完整传输，以及客户端或源站是否使用了自己的请求 ID。

这不是 OpenTelemetry span 系统：当前不生成 `traceparent`，不记录应用内部函数、数据库或下游服务 span。源站应用应把收到的 `X-Request-ID` 写入自己的日志，才能继续关联应用内部行为。

## ID 契约

| 字段 | 来源 | 行为 |
| --- | --- | --- |
| 边缘请求 ID | Nginx `$request_id` | 每个边缘请求生成唯一 ID，作为整条链路的规范 ID |
| 客户端请求 ID | 客户端请求头 `X-Request-ID` | 原样记录，但不会替代边缘规范 ID，避免客户端碰撞或伪造内部 ID |
| 源站请求 ID | 源站响应头 `X-Request-ID` | 记录源站确认或重新生成的 ID；主备重试时可能包含多次尝试 |

边缘在所有 HTTP、流式 HTTP、WebSocket 握手和 gRPC 主备回源分支中，把规范 ID 作为 `X-Request-ID` 发送给源站。客户端原有的 `X-Client-Request-ID` 等其他头不会被改写。

边缘隐藏源站响应中的同名头，并在客户端响应中统一返回规范 `X-Request-ID`，避免出现两个互相冲突的响应头。HTTP 到 HTTPS 的 301 跳转也会返回请求 ID。内部健康检查不写访问日志，也不参与业务追踪。

```text
客户端 X-Request-ID（可选）
          │ 记录为 client_request_id
          v
边缘生成规范 ID ── X-Request-ID ──> 源站应用日志
          │                              │
          │                              └─ 响应 X-Request-ID（可选）
          │                                      │
          ├─ 响应客户端 X-Request-ID             └─ 记录为 upstream_request_id
          └─ 边缘队列 -> ClickHouse -> 控制台检索
```

## 传输状态与字节

访问日志额外记录：

- `request_completion`：`OK` 表示 Nginx 已在边缘完成该请求；空值在边缘解析时转为 `INTERRUPTED`，通常表示客户端连接提前终止；升级前没有该字段的日志显示 `UNKNOWN`；
- `upstream_bytes_sent`：Nginx 向源站发送的字节；
- `upstream_bytes_received`：Nginx 从源站收到的字节；
- 主源重试或切换备用源时，源站 ID、地址、状态、耗时和字节保留 Nginx 的多次尝试序列。

`request_completion=OK` 只表示边缘 HTTP 传输完成，不表示 SSE、AI 生成任务或其他应用层工作流已经业务成功。应用层完成事件仍应由源站日志或业务指标判断。

## 查询与使用

日志页的“请求 ID”筛选同时匹配边缘 ID、客户端 ID 和源站 ID，查询范围仍受原始日志 7 天 TTL 限制。请求详情展示三类 ID、边缘传输状态、回源字节和已有的建连/首字节/完整响应耗时。

客户端可直接读取响应头并交给管理员查询：

```bash
curl -sS -D - -o /dev/null https://cdn.example.com/path
```

源站应用至少应记录收到的 `X-Request-ID`。若响应也返回 `X-Request-ID`，控制台会将其记录为源站请求 ID；最简单的做法是原样返回收到的规范 ID。

请求 ID 会进入响应头、日志和管理界面，不应包含令牌、Cookie、用户信息或其他秘密。

## 协议边界

- HTTP/1.1、TLS HTTP/2、H2C、HTTPS、gRPC/GRPCS 和 WebSocket 握手均传播规范 ID；
- WebSocket 升级后的帧属于同一连接，不会为每个帧生成独立请求 ID；
- Nginx `stream` TCP 转发没有 HTTP 请求语义，不进入这套追踪；
- 控制面不可用时，新增字段与原访问日志一起在边缘本地队列暂存，恢复后再上传。

## 升级收敛

新版 Agent 上报 `request_tracing_v1` 能力后，控制面会检查该节点当前 desired state。若节点承载 HTTP/gRPC 源站但仍是旧渲染结果，只重建该节点并增加版本；纯 TCP 节点和已经带追踪标记的配置不会重复发布。这样可以逐台升级边缘节点，不要求同时执行全量 `publish-all`。
