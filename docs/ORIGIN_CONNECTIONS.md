# 回源连接池、主动探测与熔断

HTTP、HTTPS、WebSocket 和 gRPC 站点在具备 `origin_connection_v1` 能力的边缘节点上使用托管回源连接池。旧代理没有该能力，控制面继续为其生成原有的按站点 upstream；代理升级并首次上报能力后，控制面只重建该节点的 desired state，不要求一次性升级全部节点。

## 连接池归并与容量

控制面按以下字段归并连接池：

- 代理协议：`http`、`https`、`grpc` 或 `grpcs`；
- 规范化后的连接地址和端口；URL 未写端口时，HTTP/GRPC 补 `80`，HTTPS/GRPCS 补 `443`；
- HTTP Host 请求头；
- TLS SNI。

四项完全相同的主源或备用源共享一个 Nginx upstream 和空闲连接池。Host 或 SNI 不同的虚拟主机保持隔离，避免把健康判断错误地扩散到另一个源站身份。

Nginx 的 `keepalive` 数量是每个 worker、每个 upstream 的空闲连接上限，不是全节点硬上限。控制面以节点 `worker_connections / 16` 作为每 worker 的总空闲回源预算，限制在 16-1024 之间，再按连接池被站点引用的次数分配；单池上限通常为 64，在 `worker_connections >= 16384` 时为 128。每个池至少保留 1 个槽位。实际连接总数还包括正在使用的连接，因此源站容量规划不能只看 keepalive 数量。

托管 upstream 显式使用 HTTP/1.1 空闲连接复用、45 秒空闲超时、每连接最多 1000 个请求、最长 1 小时连接寿命，以及 HTTPS 会话复用。发布相同配置不会增加 desired-state 版本。

## 主动探测与熔断

边缘代理使用两层主动探测，单次截止时间为 3 秒，最多并发探测 8 个池。每个节点和连接池使用稳定的 +/-20% 时间抖动，避免多节点同时请求同一源站：

- 服务探测：健康时以 5 秒为基准（实际 4-6 秒），复用 Agent 为该连接池维护的专用 HTTP Keep-Alive 或 gRPC `ClientConn`，用于持续确认应用路径可用；
- 冷连接探测：健康时以 40 秒为基准（实际 32-48 秒），每次新建并关闭 TCP/TLS 连接，用于发现 DNS、TCP、TLS、证书和 ALPN 等只在新建连接时暴露的问题；
- 任一层首次失败后，该池进入加速探测：服务探测以 2 秒为基准（实际 1.6-2.4 秒），冷连接探测以 5 秒为基准（实际 4-6 秒）。其他健康池仍保持各自的正常频率。

服务探测连接由 Agent 单独维护，不会占用、替换或关闭 Nginx 正在承载业务的 upstream 连接。Nginx 的实际连接复用和错误仍由访问日志指标反映。

协议行为如下：

- HTTP/HTTPS 使用源站根路径 `/` 的 `HEAD` 请求，并携带配置的 Host；完整 HTTP 5xx 响应计为失败，其他完整响应表示传输可用。
- HTTPS 使用配置的 SNI 和系统 CA 验证证书，规则与 Nginx 回源一致，不支持跳过验证或自定义 CA。
- gRPC 服务探测使用标准 Health Check RPC；返回 `SERVING` 视为正常，显式非服务状态视为失败。未实现可选 Health 服务（`UNIMPLEMENTED`）时，已建立的 gRPC 传输仍视为可用。
- gRPC 冷连接探测验证 TCP；GRPCS 还验证证书并要求 TLS ALPN 协商为 `h2`。

两层分别累计成功和失败，任一层成功都不会清除另一层的失败。状态转换使用双阈值：

```text
closed --任一层连续 2 次失败--> open
open --两层各成功 1 次--> recovering
recovering --两层各累计 2 次成功--> closed
open/recovering --任一层失败--> open，并重新累计两层恢复成功
```

进入 `open` 时，代理把该池的 include 文件原子切换为 `server ADDRESS down;`，先执行 `nginx -t`，再 reload。主源会立即返回无可用 upstream，现有主备错误页逻辑可快速转到备用源。恢复必须同时确认已有连接路径和新建连接路径，防止某一条路径的短暂成功造成抖动。

include 切换、站点配置发布和 Nginx reload 使用同一串行锁。状态先写入 `/opt/cdn-edge/data/origin-connections.json`；Nginx 校验或 reload 失败时，代理恢复旧 include、旧持久化状态和旧 worker 配置。当前池文件位于：

```text
/opt/cdn-edge/config/nginx/origin-pools/<pool-id>.conf
```

不要手工修改这些文件。删除站点或改变源站后，代理仅在新 desired state 成功接管后清理失效池文件。

## 指标与实时状态

访问日志记录 Nginx 的回源建连、收到响应头和完整响应时间；HTTPS 的建连时间包含 TLS 握手。发生 Nginx 重试或主备切换时，每次 upstream 尝试都单独计入样本和耗时，不会只统计第一段。ClickHouse 原始日志保留 7 天；`cdn_origin_minute` 将各阶段的样本数、总耗时和连接复用数预聚合并保留 30 天，站点详情显示最近 24 小时加权平均值。复用率按建连耗时为 `0 ms` 的尝试推断，因此极低延迟的新连接也可能被计为复用。部署新版本前的请求没有这些阶段字段，不做伪造或回填；新请求到达后才显示回源性能。

两层主动探测结果随 5 秒机器状态上报，在节点详情的“回源连接”区域并列展示。服务探测标明是否复用了探针连接；冷连接探测展示 TCP、TLS 和总耗时；两者都显示响应状态、错误和采样时间。控制面只在内存中保留最新快照并通过受认证 SSE 推送，不写 SQLite 或 ClickHouse；主控重启或尚无新样本时，该区域留空。

## 排查

先确认代理和 Nginx 正常，再检查托管池和最近错误：

```bash
sudo systemctl is-active cdn-edge-agent nginx
sudo /opt/cdn-edge/nginx/sbin/nginx -t
sudo /opt/cdn-edge/nginx/sbin/nginx -T 2>/dev/null | grep -A6 -F 'upstream origin_pool_'
sudo find /opt/cdn-edge/config/nginx/origin-pools -maxdepth 1 -type f -print -exec sed -n '1,3p' {} \;
sudo journalctl -u cdn-edge-agent -u nginx --since '-5 minutes' --no-pager
```

若池持续 `open`，先在节点详情确认是服务探测还是冷连接探测失败，再从边缘节点验证连接地址、Host、SNI、系统 CA、根路径 HEAD 响应或 gRPC Health 状态。不要通过手工改 include 或 applied version 绕过熔断；先修复源站或配置，再等待两层探测自动确认恢复。
