# simple_cdn 项目状态与参考技术架构

更新时间：2026-07-29（基于仓库实现和占位符部署拓扑，不记录实际生产环境信息）

## 1. 项目目标与边界

这是一个面向单管理员、小规模自用场景的自托管 CDN：一个控制面 VPS 管理约 3-10 台 Debian 边缘 VPS。需要 HTTP/3 的边缘推荐 Debian 13；Debian 12 继续使用 HTTP/1.1 与 HTTP/2。Cloudflare 只作为权威 DNS 和 DNS-01 API 提供方，业务域名保持 DNS-only；终端用户直接连接边缘节点。

当前设计边界：单管理员、IPv4、单 Cloudflare 账户、无租户/RBAC、无控制面高可用、无 GeoDNS、无 WAF/DDoS 服务、无 URL 级删除缓存。控制面故障不会中断已经下发到边缘的流量，但会阻止新发布、DNS 调整和证书续期。

## 2. 当前实施结论

代码已经具备一个可运行的端到端闭环：节点注册、mTLS 边缘通信、站点配置、DNS-01 证书、Nginx 分片配置与回滚、缓存、流式协议、WireGuard 专用回源、有界健康检查/DNS 对账、日志聚合、带重试的备份、离线/在线恢复、消息中心和批量节点升级均已实现。SQLite 使用显式版本、名称和事务边界的迁移，不再依赖启动时散落的条件式 DDL。

参考部署由一个 Compose 控制面和多台 active 边缘节点组成。边缘节点统一使用 `/opt/cdn-edge` 集中布局，并应通过全量重新发布、健康检查、DNS 对账、业务访问和日志续传验证。

## 3. 技术架构

```text
                         管理流量
管理员浏览器 ── HTTPS 443 ──> control.example.com
                              │
                              ├─ 公网反向代理层（当前为 Caddy 监听 443）
                              └─ cdn-control :${CONTROL_MTLS_PORT}
                                   │
                                   ├─ SQLite: 元数据、任务、会话、加密证书、节点当前状态
                                   ├─ ClickHouse: 原始访问日志、分钟聚合、7 天 TCP 拨测历史
                                   ├─ Cloudflare API: DNS-only A 记录对账
                                   ├─ Certbot + Cloudflare DNS 插件: DNS-01 证书签发
                                   ├─ SMTP: 告警接口
                                   └─ Restic: 控制面备份

                         边缘控制流量（mTLS）
edge-a 上的 cdn-edge-agent ── HTTPS ${CONTROL_MTLS_PORT} ──> cdn-control
       │                                      │
       ├─ 拉取 desired state / 升级任务       ├─ 节点注册/CSR 签发内部客户端证书
       ├─ 原子写入 Nginx 配置与站点证书       ├─ 心跳、健康/DNS 对账
       ├─ nginx -t -> reload / 失败回滚       └─ 接收批量访问日志
       └─ 上报心跳、5 秒机器状态、应用版本、代理摘要、日志

                         业务请求路径
客户端 ── Cloudflare DNS-only A ──> Edge Nginx TCP :80/:443、按站点启用的 QUIC UDP :443
                                          │
                                          ├─ 磁盘缓存 / stale 回退
                                          ├─ WebSocket / SSE 流式代理
                                          ├─ gRPC 原生代理
                                          └─ 主源站 -> 可选备用源站
                                             └─ 可选 WireGuard 私网回源
```

### 3.1 控制面

- 语言与运行时：Go 1.26，`modernc.org/sqlite` 驱动的 SQLite。
- 管理认证：Argon2id 密码、TOTP、一次性恢复码、HttpOnly 会话 Cookie、CSRF 令牌、登录限流与审计记录。
- 部署模型：站点先创建和签发证书，再显式发布；发布生成每个边缘节点的 desired state，不直接 SSH 修改边缘。
- 证书：Certbot `dns-cloudflare` DNS-01，私钥以 `CONTROL_ENCRYPTION_KEY` 进行 AES-GCM 加密后保存在 SQLite，向边缘仅通过 mTLS 下发。
- 节点健康：每 15 秒启动一轮有截止时间的对账，以固定上限 worker 并发探测并串行写入状态；连续 3 次失败从 DNS 池移除，连续 5 次成功恢复；所有节点均不健康时保留现有 DNS，不发布空记录。每轮聚合全部错误、记录耗时并通过诊断 API 暴露最近结果，上一轮未结束时不会重叠启动下一轮。
- 日志与指标：ClickHouse 原始请求日志保留 7 天，请求量与回源阶段耗时分钟聚合保留 30 天；边缘控制面不可达时，本地队列暂存访问日志。访问日志包含回源建连、响应头和完整响应时间，站点页显示最近 24 小时连接复用率及按各自样本加权的平均值。5 秒机器状态和主动回源探测只在主控内存中保留最新值，不持久化历史。
- TCP 监测与智能路由：拨测目标支持唯一名称；SQLite 保存最新评分、目标结果及每节点智能路由策略。评分门控支持双阈值和独立连续轮数，时间门控支持 `Asia/Shanghai` 每周重复窗口，两者同时启用时采用 AND 关系。每轮目标明细在 SQLite 事务提交后进入有界内存队列，由后台批量写入 ClickHouse 并保留 7 天；ClickHouse 写入或查询故障不会阻塞节点上报，也不会清空 SQLite 当前状态。具体规则见 [SMART_ROUTING.md](SMART_ROUTING.md)。
- WireGuard 回源：SQLite 迁移 v24 持久化隧道、Peer、一次性源站安装令牌和性能任务；管理 API 受管理员会话与 CSRF 保护，边缘配置、状态和任务接口沿用节点 mTLS，唯一公开的源站配置接口只接受 15 分钟高熵令牌。发布器在源站与全部 Peer 修订收敛后才将连接主机改写为私网 IP，不提供隐式公网回退。

### 3.2 边缘面

- Edge agent 用 15 分钟的一次性注册令牌提交 CSR，获得控制面内部 CA 签发的 mTLS 客户端证书；后续所有状态拉取、心跳、日志上传均使用该证书。
- 边缘自有文件统一位于 `/opt/cdn-edge`：`bin/`、`config/`、`data/`、`logs/`、`cache/` 和 `systemd/`。系统路径仅保留 systemd 与 Nginx 的发现链接；Nginx 包、全局配置和 journald 仍由 Debian 管理。
- Agent 默认每 30 秒发送一次独立心跳，并每 5 秒通过同一个 mTLS HTTP/2 Transport 上报机器状态。主控在心跳响应中合并返回期望状态、监控目标、安全封禁和升级任务修订；边缘仅拉取变化项，慢速配置、拨测和升级任务不会阻塞心跳或机器状态采集。配置或证书写入采用原子替换，先执行 `nginx -t`，仅在成功时 reload，失败会恢复上一个已知可用版本。
- 主控仅接受采集时间更新的机器快照，只在内存中保存每节点最新值，并通过受管理员会话保护的 SSE 推送到节点详情页，不写入持久化存储。快照可选包含从节点本地 Unix socket 采集的 Nginx 活动连接、读写/等待状态和累计请求；Nginx 不可用时该区域留空。SSE 每 15 秒复核会话并发送保活，前端以 30 秒 GET 作为断线回退，且按采集时间避免旧响应覆盖新事件；没有快照时不显示机器状态区域。
- `origin_connection_v1` 节点按协议、规范化地址、Host 和 SNI 共享 upstream；每 worker 空闲连接预算由 `worker_connections` 自动分配。Agent 分层探测源站：约 5 秒复用服务连接，32-48 秒新建一次 TCP/TLS；任一层首次失败后加速到约 2 秒和 4-6 秒，单次超时 3 秒。两层分别累计，任一层连续 2 次失败把托管 include 切换为 `down`，两层分别连续成功后恢复。池状态在边缘持久化以跨重启维持熔断，但主控只接收实时快照。详见 [ORIGIN_CONNECTIONS.md](ORIGIN_CONNECTIONS.md)。
- `wireguard_v1` 节点在本地生成并以 `0600` 持久化 Curve25519 私钥，按 ETag 拉取隧道期望状态，经 `wg-quick` 原子应用或回滚并上报握手、流量、修订与错误。`wireguard_performance_v1` 节点串行执行公网 TCP、隧道 TCP 和隧道 UDP `iperf3` 测试。源站由一次性脚本安装 systemd 服务、受管配置和按边缘公网 IPv4 收敛的 nftables 规则。详见 [WIREGUARD_ORIGIN.md](WIREGUARD_ORIGIN.md)。
- Agent 上报自身 SHA-256 和 `online_upgrade_v1` 能力；主控可对单节点下发当前制品，独立 updater 在替换主进程后等待新 Agent 完成 mTLS 心跳，失败恢复旧二进制和 systemd/Nginx 集成。
- 安全工作台管理全局请求路径策略、活动 IP 封禁和最近命中；Nginx 在回源前按正则返回 444，Agent 使用自有 nftables 表即时封禁 TCP 80/443 与 QUIC UDP 443，并通过 mTLS 在节点间同步和自动过期。
- Nginx 为 HTTP 与 stream 分别生成共享 `00-base.conf` 和每站点 `site-<id>.conf`；Agent 使用版本与内容摘要目录同时暂存两个配置族，再原子切换稳定索引。每个站点仍拥有独立 `server` 和 `upstream`：80 强制跳转 HTTPS，TCP 443 保留 TLS 1.2/1.3 与 HTTP/1.1/2；站点默认关闭 HTTP/3，只有主动开启且节点报告 `http3_v1` 时才额外监听 UDP 443、广告 `Alt-Svc` 并启用 QUIC 地址验证重试。Agent 在应用前后分别检查 TCP/UDP 端口归属，站点设置发布或节点能力变化会触发节点期望状态自动重建。节点级 `worker_processes`、`worker_connections` 和 `worker_rlimit_nofile` 分别由主配置与 `events` include 管理。
- CDN 业务 HTTP/HTTPS server 默认显式使用 `keepalive_timeout 120s` 和 `keepalive_requests 1000`，并可按站点覆盖客户端保活秒数；每个 upstream、每个 worker 的空闲回源连接池为 `keepalive 30`，HTTP/gRPC 回源连接超时统一为 10 秒。HTTP/HTTPS 源站默认 HTTP/1.1，并可在新版节点上分别选择 H2C 或 TLS HTTP/2；WebSocket Upgrade 始终使用协议隔离的 HTTP/1.1 upstream，SSE 和 POST 继续进入无缓存分支。主配置启用 PCRE JIT 和最长一小时的旧 worker 排空，QUIC host key 跨升级保留，TLS session cache 超时为 30 分钟。

### 3.3 请求处理策略

- 普通 HTTP(S)：只有 CSS、JavaScript、字体、图片、WebAssembly 和 Web Manifest 等常见静态后缀的 GET/HEAD 会选择缓存，其他 URI 使用 `proxy_cache off`；缓存键包含 `site_id` 和 `cache_generation`。携带 Authorization 或 Cookie 的请求不会读取或写入共享缓存，但这些请求头仍会原样回源；Nginx 同时遵循源站响应自带的缓存控制语义。
- 磁盘缓存：所有站点共享节点缓存区，全局默认总上限为 1 GiB/节点，可在节点详情页单独覆写；`keys_zone` 大小按有效节点缓存上限和启用缓存的站点数计算，并限制在 16-512 MiB；7 天非活跃回收，并启用 cache lock、revalidate、后台刷新和上游错误时 stale 回退。
- 整站透传：HTTP(S) 站点可选启用，关闭 Nginx 缓存、请求缓冲和响应缓冲，使用站点配置的读写空闲超时，并显式转发 `Range` / `If-Range`，适用于视频及其他按字节范围读取的流量。操作语义、已验证故障根因和验收命令见 [PASSTHROUGH_MODE.md](PASSTHROUGH_MODE.md)。
- 请求体上限：业务站点默认 `128 MiB`，可按站点选择 `128 / 256 / 512 / 1024 MiB` 四个档位；修改后需重新发布才能进入边缘 Nginx 配置。
- 回源读写空闲超时：HTTP/HTTPS/WS/WSS 默认 120 秒，可按站点选择 `120 / 360 / 900 / 1800 / 3600` 秒；已有站点的自定义值保持不变。按 [Nginx `proxy_read_timeout` / `proxy_send_timeout` 语义](https://nginx.org/en/docs/http/ngx_http_proxy_module.html#proxy_read_timeout)，它约束连续两次回源读写之间的空闲间隔，不限制持续有数据的连接总时长。主源、备用源、普通和流式分支使用同一值。
- WebSocket/SSE：无需配置路径。`Upgrade: websocket`、包含 `text/event-stream` 的 `Accept`、`X-CDN-Stream: 1` 以及所有 POST 请求会自动关闭缓存和响应缓冲；控制头不会转发到源站。POST 自动直通兼容在 JSON 请求体中使用 `stream: true`、但不发送 SSE Accept 头的 [OpenAI 流式接口](https://developers.openai.com/api/docs/guides/streaming-responses)。
- 流式路径兼容：API 暂时保留 `stream_paths` 字段；旧客户端提交的值会被忽略，响应固定为空数组，SQLite 中的历史路径值会在打开数据库时清空，Nginx 不再生成路径专用 location。
- gRPC：`grpc://`/`grpcs://` 源站使用 Nginx `grpc_pass`，客户端经 HTTP/2 接入，默认不缓存，支持 1 小时 gRPC 读写超时和可选备用源站。
- 源站容错：主/备源站支持连接、超时、无效响应和 5xx 的切换；HTTPS/gRPCS 源站启用 SNI 和 CA 校验。
- WireGuard 回源：站点的主/备源站可分别选择覆盖其全部节点的隧道。发布时仅替换 URL 连接主机并保留端口、Host、SNI 和应用协议；HTTP/H2C 或 `grpc://` 可取消隧道内 TLS，HTTPS/GRPCS 继续执行证书校验。

## 4. 代码与模块状态

| 模块 | 状态 | 说明 |
| --- | --- | --- |
| `cmd/control` | 已实现 | 控制面进程；支持 `keygen` 和仅本机使用的 `publish-all`。 |
| `internal/control` | 已实现 | HTTP API、认证、证书任务、发布、WireGuard 编排、DNS 健康对账、审计、嵌入式管理界面。 |
| `internal/store` | 已实现 | 版本化事务迁移、SQLite schema、站点/节点/任务/消息/会话/证书/状态持久化。 |
| `internal/edge` | 已实现 | 注册、mTLS、配置同步、WireGuard 对账与性能任务、原子应用、Nginx 回滚、心跳、日志转发、5 秒机器状态和低频缓存磁盘占用采集。 |
| `internal/nginx` | 已实现 | HTTP 缓存、整站透传、回源、TLS、WebSocket/SSE、gRPC、备用源站与客户端 IP 限速配置渲染。 |
| `internal/integrations` | 已实现 | Cloudflare、Certbot、SMTP 等外部适配器。 |
| `internal/logstore` | 已实现 | ClickHouse 原始日志、分钟指标、节点缓存状态聚合，以及 7 天 TCP 拨测历史及异步批量写入。 |
| `frontend/` 与 `internal/control/web/dist` | 已实现 | React 19、Vite、TypeScript、Tailwind CSS v4 和 shadcn/ui 简体中文管理台；Vite 哈希产物通过 `//go:embed web/dist` 编入控制面二进制。 |
| `deploy/` 与 `scripts/` | 已实现 | Compose 主控安装、证书续期、Restic 重试/状态/告警、隔离恢复与切换、发布构建和 ClickHouse 配置。边缘安装资源由控制面从 `internal/control` 嵌入提供。 |

### 管理界面当前能力

- 概览：节点数、运行节点数、站点数、最近 24 小时请求量/传输量/错误率/状态码；站点请求趋势支持按站点、请求总量或传输量升降序排列，条目可进入独立分析页查看站点请求量、传输量、错误汇总、状态码分布和分时折线图。
- 日志：从左侧导航进入，默认检索全部站点最近 1 小时原始日志，支持时间、站点、节点、方法、状态码、路径、客户端 IP、缓存状态筛选和每页 100 条手动分页；原始日志保留 7 天。
- 安全：全局访问策略增删改、内置敏感文件扫描与独立 PHP 恶意文件探测规则、拦截/IP 封禁动作、1 小时至 7 天档位，以及按客户端 IP 执行的每秒请求限速策略；限速可选择仅由 2xx/3xx/4xx/5xx 响应计数，超限由边缘返回 429。仅以 4xx/5xx 为条件的策略还可配置连续 429 次数，达到阈值后沿现有边缘事件链封禁 IP。页面同时展示访问安全/限速能力覆盖、活动封禁解封和最近命中。
- 监测：命名 TCP 拨测目标支持新增、重命名、启停和删除；节点列表保留当前评分、成功率、时延和连续异常，点击节点进入独立历史页。历史页可在 `1h / 6h / 12h / 24h / 7d` 间切换服务端聚合区间，并用可勾选图例将多个目标的时延曲线绘制在同一图中。“智能路由”Tab 可按节点编辑评分滞回和每周允许调度窗口，并显示当前阻断原因与下次切换时间。
- 节点：列表仅保留运行概览和管理入口，并提供“一键升级全部”；后端一次评估全量节点，只为具备能力且制品落后的可用节点排队，逐节点报告已是最新、已有任务或阻塞原因。独立二级页面集中提供部署/升级命令、在线升级、暂停/启用调度、撤销/重新启用、卸载/删除、分配站点、节点缓存总配额覆写、心跳、能力与应用版本查看，并展示边缘心跳上报的发行版、版本、uptime、系统负载、CPU、RAM、根磁盘和默认出口网卡 RX/TX，以及缓存已用空间/节点有效总配额和最近 24 小时 ClickHouse 缓存状态分布。机器状态、缓存磁盘上报与请求统计独立降级，任一故障不阻塞节点管理。
- 站点：创建后自动申请 TLS、编辑、节点分配、主/备源站、独立回源 TLS SNI、HTTP/2/H2C 与 WireGuard 回源链路、回源读写空闲超时、整站透传开关、带 TLS 签发门禁的发布、缓存刷新、源站 CIDR 查看，以及输入站点名确认的安全删除流程。
- WireGuard：隧道增删改、节点能力筛选、源站一次性安装/卸载命令、修订收敛、Peer 公钥/握手/流量/错误状态，以及公网 TCP、隧道 TCP、隧道 UDP 性能对照。
- 站点列表采用紧凑工作台布局，仅展示节点、TLS 与发布状态并保留管理入口；创建、编辑、发布、协议、缓存、请求体、超时、TLS、缓存刷新和源站 CIDR 均集中在独立二级页面。
- TLS 状态不再解析历史任务文本。接口 `GET /api/sites/{id}/tls-status` 返回最新证书任务及 `published_after_certificate`，只要签发完成后存在成功发布任务就显示“已签发”。
- 消息中心：顶部单行任务提示已移除；侧栏和移动端铃铛显示未读数，支持全部/未读筛选、逐条/全部已读和删除。发布、节点升级、卸载、备份及在线恢复的关键状态会按来源和状态去重持久化，已读消息保留三个月；当前浏览器操作结果使用 Sonner 即时通知。
- 设置：通用设置支持品牌名称、副标题及 PNG/JPEG Logo，并同步侧栏、登录页和浏览器标题/favicon；同时显示最近一次备份运行状态和 Restic 快照，可在不中断控制面的阶段下载、校验 SQLite/密钥/证书并恢复到临时 ClickHouse 数据库；二次确认后短暂重启完成切换，旧文件和旧数据库保留用于回滚。

## 5. 参考部署模板

以下值仅用于说明部署关系。实际主机、域名、端口、版本和运行状态应从受控的运维系统实时读取，不应提交到 Git。

| 项目 | 当前值 |
| --- | --- |
| 控制面主机 | `control-host` |
| 管理域名 | `https://control.example.com` |
| 控制面监听 | Compose `control` 使用 host network 监听 `${CONTROL_MTLS_PORT}`；公网 443 由共享反向代理接入。 |
| 控制服务 | Compose `control`、`clickhouse`、`control-cert-renew` 应保持运行并通过健康检查。 |
| 数据目录 | 统一根目录 `/opt/cdn-platform`；配置、control 数据、ClickHouse、证书、日志、备份和 rollback 均在其下。 |
| ClickHouse | Compose `clickhouse` 使用 `26.6` 分支浮动标签，跟随该分支的最新补丁；待 `26.8` 正式标记为 LTS 并通过升级验证后切换。HTTP 仅映射到 `127.0.0.1:${CLICKHOUSE_HTTP_PORT}`。 |
| ClickHouse 验收 | 原始访问日志、分钟聚合和 `cdn_tcp_monitoring_history` 应持续写入；拨测历史 TTL 为 7 天，具体数量不记录在仓库中。机器状态不写入 ClickHouse。 |
| TLS | Compose 内使用 Cloudflare DNS-01 独立签发并每 12 小时检查续期；主控支持无重启热加载。 |
| 备份 | SQLite、ClickHouse 和 Restic 工作流应通过隔离恢复演练；凭据只保存在部署环境。 |
| 边缘节点 | `edge-a`（`203.0.113.10`）、`edge-b`（`203.0.113.11`）、`edge-c`（`203.0.113.12`）、`edge-d`（`203.0.113.13`）代表 RFC 5737 示例节点。 |
| 边缘应用状态 | 所有 active 节点的 `applied_version` 应与目标版本一致且 `last_error` 为空。 |
| Edge 服务 | `cdn-edge-agent.service` 和自管 `nginx.service` 均为 `active`，`/opt/cdn-edge/nginx/sbin/nginx -t` 成功。 |
| Edge 目录迁移 | 所有边缘节点都应使用 `/opt/cdn-edge`；旧路径不得出现在 desired Nginx 配置中。 |
| 示例站点 | `api_example_com`、`app_example_com`、`node_example_com`、`stream_example_com` 代表已启用并发布的站点。 |

### 参考网络与安全分层

- 管理 UI/API 走 `control.example.com` 的公网 HTTPS 地址。
- Edge agent 使用 `EDGE_CONTROL_URL=https://control.example.com:${CONTROL_MTLS_PORT}` 直接进行 mTLS 通信，不经过公网管理反向代理。
- 控制容器使用 UID/GID `10001`，仅写 `/opt/cdn-platform/data/control` 和 Certbot 日志；ClickHouse 数据由 UID/GID `101` 持有。
- Edge agent 以 root 运行以写入 Nginx 配置、站点证书和执行 reload；其 systemd 服务同样设置 `LimitNOFILE=65536`。
- Edge 持久备份边界为 `/opt/cdn-edge/config` 与 `/opt/cdn-edge/data`；缓存可重建，访问日志在已上传到主控后可不纳入节点备份。
- Cloudflare API 令牌、控制面加密密钥等均在 `/opt/cdn-platform/config/control.env`，文档和日志中不应记录其值。

## 6. 已完成的近期修复

1. DNS-01 签发任务与控制面重启：签发任务在进程生命周期外持久化；控制面重启会将进行中的任务标为失败并要求显式重试，避免重复 ACME 操作。
2. Certbot 重复 TXT 记录：签发实现已处理此前排查出的重试和残留记录路径；应使用 `api.example.com` 等测试域名完成验收。
3. WebSocket、SSE、gRPC：WebSocket/SSE 已改为全路径自动识别，OpenAI 风格 POST 流式响应自动直通；gRPC 使用专用源站协议。
4. 缓存配额：配额从按站点累加改为节点总量；全局默认 1 GiB/节点，节点详情页可按磁盘容量单独覆写，避免小磁盘节点因站点数量增加而扩大缓存上限。
5. 上游连接复用：普通主源和备用源 location 显式清空 `Connection` 头，避免 upstream keepalive 被默认 `Connection: close` 破坏；当前连接池固定为每个 upstream、每个 worker 30 个空闲连接。
6. 重新发布运维入口：新增 `cdn-control publish-all`，在 Nginx 渲染器升级后可重新构建全部已发布站点的 desired state，不需要直接编辑 SQLite 或绕过管理认证。
7. TLS UI 状态：改为签发任务与后续成功发布任务的时间线判断；消除了“实际已发布但仍显示已签发，待发布”的错误展示。
8. 管理台中文化与站点页重构：完成简体中文适配，并优化站点页面的密度、状态层级和操作菜单。
9. HTTP(S) 整站透传与 Range 修复：新增 `passthrough` 持久化字段、管理台开关、旧库迁移和 Nginx 无缓存渲染。开启后普通主/备源站位置关闭缓存与请求/响应缓冲，显式转发 `Range` / `If-Range`；完整验收方法见 [PASSTHROUGH_MODE.md](PASSTHROUGH_MODE.md)。
10. Edge 目录收敛：新装、旧布局迁移和后续升级统一使用 `/opt/cdn-edge`；迁移保留节点身份、证书、应用版本和日志队列，清空可重建缓存，并在 Nginx 或 Agent 验证失败时恢复旧服务状态。完整流程见 [EDGE_DEPLOYMENT.md](EDGE_DEPLOYMENT.md)。
11. 站点安全删除：删除请求先撤销精确归属的 Cloudflare 记录，再等待已分配 active 节点确认移除 Nginx 配置和证书，最后清理 Certbot lineage 与 SQLite 元数据；失败时保留停用的删除中状态并支持重试，审计、任务和按 TTL 保留的访问日志不受影响。
12. Nginx reload 假成功：旧目录迁移改变 `cdn_cache` 路径时改为冷重启；Agent reload 后必须观察到新 worker 才确认应用成功，否则回滚且不推进 applied version。根因、restart 边界和验收命令见 [NGINX_APPLY_SAFETY.md](NGINX_APPLY_SAFETY.md)。
13. 站点级访问健康：控制面除节点 HTTP 探测外，新增直连节点 443、保留真实域名 Host/SNI 并校验证书和站点专属响应体的检查；按站点和节点独立执行 3 次失败摘除、5 次成功恢复，避免端口正常但虚拟主机仍错误时继续把该节点视为健康。
14. Edge 在线升级：节点页按代理 SHA-256 判断是否落后，通过 mTLS 下发单节点任务；制品预检、独立 systemd updater、新代理心跳 readiness、持久化结果补报和失败回滚组成完整闭环。存量旧代理需最后手动升级一次以获得该能力。
15. 通用访问安全：控制端持久化有序正则策略，Nginx 命中后在回源前关闭请求；边缘 Agent 将公网 IPv4 写入带超时的 nftables 集合，本地先执行、控制端校验记录、全节点同步，并提供手动解封。
16. 健康对账调度：改为固定上限 worker、单轮超时、错误聚合和耗时快照；超时后不再提交迟到探测的状态变更，定时器不会产生并行轮次。
17. 数据库升级：引入 `schema_migrations`，每个版本独立事务执行并校验版本连续性、名称和目标版本；当前迁移覆盖核心 schema、任务约束、发布/安全、Nginx 分片、消息中心和持久化消息删除。
18. Nginx 配置分片：共享 base、HTTP 站点和 stream 站点分别落盘到内容摘要目录，稳定入口只负责 include；两个配置族与证书共同纳入验证、应用和回滚。
19. 备份与恢复：定时备份增加短期重试、原子状态文件、消息中心状态和最终 SMTP 告警；离线脚本可在线预下载、恢复到临时 ClickHouse 并执行 `--verify-only` 演练，切换失败反向回滚并保留旧数据。
20. S3 在线恢复：设置页可选择精确 Restic 快照，异步阶段化校验并恢复临时 ClickHouse；提交时用跨容器读写锁排空备份/证书作业，重启前后复核摘要并切换 SQLite、CA、证书和 ClickHouse，失败闭锁等待人工处置。
21. 管理工作台：引入持久消息中心并移除页面顶部单行任务状态；节点列表新增全量升级及逐节点结果；颜色、焦点、密度、抽屉、分段筛选和移动端布局按可组合组件思路统一。
22. TCP 拨测历史：目标增加唯一名称；最新评分、连续异常和调度状态继续由 SQLite 事务维护，每轮明细通过有界非阻塞队列异步写入保留 7 天的 ClickHouse 表。节点行进入历史详情，可切换五档范围并在同一曲线图选择多个目标；ClickHouse 不可用时当前状态和边缘上报独立降级。
23. WireGuard 专用回源：新增能力门控的源站隧道、一次性安装、修订收敛、站点主/备回源选择、无公网回退发布门禁、Peer 状态和三路 `iperf3` 对照；源站可在隧道内使用 HTTP/H2C 或 `grpc://` 取消 TLS 证书管理，完整边界与验收见 [WIREGUARD_ORIGIN.md](WIREGUARD_ORIGIN.md)。

## 7. 当前问题、风险与下一步

### P0：验证源站可用性

以 `api.example.com`、示例上游 `203.0.113.11:${ORIGIN_APP_PORT}` 为模板，分别验证端口监听、响应头读取和完整业务请求。仓库不记录实际源站地址、内部端口或实时故障状态。

处理顺序：先确认源站应用及其健康状态，修复或切换到可用 URL；随后从边缘和公网各发起一次请求验证。不要将源站问题归因于边缘证书、TLS 状态或 DNS。

### P1：完成可用性与运维闭环

- 配置 `/opt/cdn-platform/config/backup.env`、Restic 密码和 S3 兼容存储凭据，初始化仓库并启用 Compose `backup` profile；隔离备份恢复已通过，生产仓库尚未配置。
- 使用现有两个独立边缘节点完成 DNS 健康摘除/恢复、源站 CIDR 白名单与故障切换演练。
- 使用真实边缘和源站完成 WireGuard UDP 端口、云安全组、MTU、重启持久性、密钥与修订收敛演练，并在高峰/低峰分别记录公网 TCP、隧道 TCP 与隧道 UDP 的带宽、丢包和抖动；代码路径已具备，线路是否存在 UDP 限流只能通过目标运营商实测确认。
- 对 Cloudflare DNS 记录、站点源站防火墙和控制面 `${CONTROL_MTLS_PORT}` 访问规则做一次上线检查，确保业务记录为 DNS-only，源站仅放行边缘 IP。
- 为 ClickHouse 磁盘容量和日志写入失败配置外部告警，并实际触发一次备份最终失败来验收状态文件、消息中心和 SMTP 告警；代码路径已具备，生产通知链路尚需环境演练。

### P2：连接与容量调优

- 节点默认使用 `worker_processes auto`、`worker_connections 4096` 和 `worker_rlimit_nofile 65536`；Node 详情可按 VPS 的文件描述符、内存和源站能力调整三项值。`sendfile on`、`tcp_nopush on` 等全局参数由 `/opt/cdn-edge/nginx/conf/nginx.conf` 统一管理。
- HTTP/3/QUIC 已实现按站点选择并默认关闭：安装器仅在 Nginx 包含 `ngx_http_v3_module` 时声明能力，主动开启的站点仍需节点能力门控；配置保留 HTTP/1.1/2 回退，并将 UDP 443 冲突、监听确认、回滚和 IP 封禁纳入既有事务。上线时仅需为承载已开启站点的节点确认云厂商与主机防火墙开放 UDP 443，并从外部执行 `curl --http3-only` 验收。
- 可配置的客户端保活和 2-60 分钟回源读写空闲超时不能代替应用层保活。WebSocket 应发送 ping/pong，SSE 应定期发送注释或事件心跳；持续有数据的连接总时长不受该档位限制。

### P3：产品与安全边界

- 当前无 RBAC、多租户、API token、完整 WAF、URL 级 purge、跨控制面高可用或自动扩缩容；已有的路径阻断、IP 封禁和节点本地请求限速属于明确边界内的轻量安全能力。
- 发布任务成功表示 desired state 已生成；DNS 放量还取决于边缘应用版本、健康检查阈值和 Cloudflare DNS 对账周期。面向生产时可在 UI 中继续增加“节点已应用/健康/DNS 已更新”的组合状态。
- Cloudflare Python SDK 的 Certbot 插件仍会打印版本升级警告；签发已成功，但应在维护窗口固定兼容版本或升级到受支持的主版本，避免未来自动升级造成 DNS-01 回归。

## 8. 开发、发布与验证流程

### 本地验证

```bash
cd /path/to/simple_cdn
npm --prefix frontend ci
npm --prefix frontend run check
npm --prefix frontend run test:e2e

GOCACHE=/tmp/simple_cdn_go_cache \
GOMODCACHE=/tmp/simple_cdn_go_modcache \
go test ./...
```

前端源码在 `frontend/`，`npm run build` 将路由分块和静态资源输出到 `internal/control/web/dist/`，再由 `internal/control/server.go` 使用 `//go:embed web/dist` 编入 `cdn-control`。因此修改 UI 后必须重新构建前端、重新编译并重启控制面，单独上传静态文件不会生效。Dockerfile、`scripts/build-release.sh` 与 GitHub Actions 已串联这两段构建。

### 发布控制面

1. 合并到 `main` 后等待 GitHub Actions 的编译、测试、浏览器检查和镜像构建全部通过。
2. Actions 只将镜像推送到 GHCR 并记录精确 digest；仓库外的私有部署配置把 digest 写入控制主机 `.env`，控制主机只执行 pull 和 Compose recreate，不执行构建。
3. 部署脚本检查运行镜像 ID、`docker compose ps` 和控制面健康；失败时自动恢复上一镜像和 Compose 文件。
4. 需要更新已发布站点的 Nginx 渲染逻辑时，执行 `docker compose run --rm --no-deps control publish-all`，使 edge agent 拉取新的 desired state。

### 发布边缘与站点

1. 在节点页创建节点，或为现有节点获取部署/升级命令，并在边缘 root 环境运行；详细目录及迁移约束见 [EDGE_DEPLOYMENT.md](EDGE_DEPLOYMENT.md)。
2. 等待节点成功注册、心跳和 Nginx 健康检查。
3. 创建/更新站点，完成 DNS-01 TLS 签发，再点击“发布”。
4. 检查节点的 `applied_version` 追上 `node_states.version`，确认 `nginx -t`、新 Nginx worker、通用 `http://EDGE_IP/__cdn_health`、站点 `curl --resolve` 专属健康响应、证书和源站请求。完整命令见 [NGINX_APPLY_SAFETY.md](NGINX_APPLY_SAFETY.md)。
5. 在源站安全组中仅放行“源站 CIDR”接口返回的边缘地址。

### 关键实时检查

```bash
# 控制面
cd /opt/cdn-platform
sudo docker compose ps
curl -fsS https://control.example.com/healthz

# 控制面数据库中的节点和站点状态
sudo sqlite3 -header -column /opt/cdn-platform/data/control/control.db \
  'select name, public_ipv4, status, applied_version, last_error from nodes;'

# 边缘
sudo systemctl status cdn-edge-agent nginx
sudo /opt/cdn-edge/nginx/sbin/nginx -t
curl -fsS http://127.0.0.1/__cdn_health
```

## 9. 关键文件索引

| 路径 | 用途 |
| --- | --- |
| `cmd/control/main.go` | 控制面启动、环境变量、ClickHouse、证书与健康任务、`publish-all`。 |
| `cmd/edge-agent/main.go` | 边缘 agent 启动和运行参数。 |
| `internal/control/server.go` | API 路由、认证保护、TLS 状态接口、嵌入静态资源。 |
| `internal/control/publisher.go` | 站点发布、desired state 和证书下发。 |
| `internal/control/wireguard.go` | WireGuard 管理/边缘 API、一次性源站命令和性能任务。 |
| `internal/control/certificates.go` | 异步 DNS-01 签发与续期。 |
| `internal/control/health.go` | 健康检查、Cloudflare DNS 对账。 |
| `internal/edge/agent.go` | mTLS 注册、同步、原子应用和回滚。 |
| `internal/edge/wireguard.go` | 边缘密钥、配置对账、状态读取和 `iperf3` 执行。 |
| `internal/nginx/render.go` | Nginx 缓存、流式、gRPC、TLS、回源配置生成。 |
| `internal/store/migrations.go` | SQLite 版本化事务迁移和 schema 版本检查。 |
| `internal/store/store.go` | SQLite 连接、生命周期和持久化入口。 |
| `internal/logstore/clickhouse.go` | ClickHouse schema、访问日志和指标查询。 |
| `frontend/` | React/Vite/Tailwind/shadcn 中文控制台源码、组件注册表和 Playwright 响应式测试。 |
| `internal/control/web/dist/` | Vite 生成并由 Go 嵌入的哈希静态产物。 |
| `deploy/docker-compose.yaml` | 主控、ClickHouse、证书续期和可选备份服务的唯一受版本控制模板。 |
| `scripts/install-control-compose.sh` | Compose 主控目录初始化与安装。 |
| `internal/control/install-edge.sh` | 控制面嵌入并提供的边缘新装、旧布局迁移与升级脚本。 |
| `scripts/compose-backup-run.sh` | 备份重试、原子状态记录与最终失败告警包装。 |
| `scripts/compose-backup.sh` | SQLite、ClickHouse、内部 CA、证书和控制配置的 Restic 备份核心步骤。 |
| `scripts/restore-control-compose.sh` | 从 Restic 快照验证、临时恢复、切换及回滚完整主控数据。 |
| `docs/NGINX_APPLY_SAFETY.md` | Nginx reload/restart 边界、新 worker 验证、站点 HTTPS/SNI 健康与故障处理。 |

## 10. 恢复开发时的第一步

1. 先运行本地完整测试，并检查 `control-host` 的控制面、ClickHouse、节点心跳和源站响应。
2. 优先解决 `${ORIGIN_APP_PORT}` 源站超时，恢复真实请求链路。
3. 配置生产 Restic 仓库并启用定时备份，再使用现有两个独立边缘节点演练 DNS 健康切换。
4. 后续功能改动遵循“先测试 -> 最小实现 -> 本地完整测试 -> 构建部署 -> 实际节点验证”的顺序；渲染器改动后使用 `publish-all` 重新生成已发布站点状态。
