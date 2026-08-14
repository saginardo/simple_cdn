# simple_cdn 项目状态与参考架构

最后审计：2026-08-09（基于当前 `main`，不代表任何生产主机的实时状态）

## 1. 当前结论

simple_cdn 是面向单管理员、小规模自用场景的自托管 CDN：一个 Docker Compose 控制面管理多台 Debian 12/13 边缘节点。Cloudflare 只承担权威 DNS 和 DNS-01 接口，业务记录保持 DNS-only，终端用户直接连接边缘 Nginx。

当前代码已经形成完整闭环：管理员初始化和认证、节点注册与 mTLS、站点草稿/证书/发布、Nginx 配置生成与回滚、缓存和压缩、WAF/PoW/限速/封禁、托管静态资源、HTTP/3、HTTP/2/H2C、gRPC、stream TCP、WireGuard 回源、监控与智能路由、日志聚合、备份/恢复、消息中心以及边缘在线升级。

Nginx 有独立的交付生命周期：控制镜像内置 bundle 只负责首次安装和兜底，GitHub Actions 每日构建官方 stable 的独立 Release，主控下载并校验后由管理员批准，再使用现有节点升级事务下发。该流程不需要为每个 Nginx 版本发布控制镜像。

## 2. 稳定边界

- 单管理员、IPv4、单 Cloudflare 账户；没有 RBAC、多租户、GeoDNS、控制面高可用、托管 CAPTCHA/机器人信誉或流量型 DDoS 服务。
- 控制面不可用不会中断已经应用到边缘的业务流量，但会暂停新发布、DNS 对账、证书任务、节点同步、资源上传、缓存操作和 Nginx 候选检查。
- 支持 Linux AMD64 的受管 Nginx bundle；边缘支持 Debian 12 和 Debian 13，Docker 只用于控制面和 CI 构建。
- WAF、PoW、限速和封禁补充而不是替代应用认证、输入校验、协议解析和上游 DDoS 防护。
- 缓存失效通过缓存键代际立即隔离旧对象，不承诺同步删除磁盘文件；历史 Nginx 工件也不会自动删除，以保证在途任务 URL 稳定。

## 3. 运行拓扑

```text
管理员浏览器
    |
    +-- 公网 HTTPS / 可选反向代理 --> cdn-control（Compose host network）
                                          |
                                          +-- SQLite: 元数据、任务、会话、加密证书、节点状态
                                          +-- ClickHouse: 访问日志、分钟聚合、拨测历史
                                          +-- Cloudflare DNS / Certbot DNS-01 / SMTP
                                          +-- Restic: 控制面备份与恢复
                                          +-- GitHub Releases: Nginx stable 上游
                                          +-- $CONTROL_DATA_DIR/nginx-artifacts
                                          +-- /downloads/nginx/<sha256>/...

终端用户 --> Cloudflare DNS-only A --> Edge Nginx :80/:443、可选 QUIC UDP :443
                                         |
                                         +-- 缓存、压缩、WAF/PoW/限速、静态资源
                                         +-- HTTP/WS/SSE/gRPC/stream
                                         +-- 主源站 -> 可选备用源站 -> 可选 WireGuard 私网

cdn-edge-agent -- mTLS --> 控制面 desired state、心跳、日志、升级任务
```

## 4. Compose 服务与数据所有权

| 服务/目录                                       | 所有权与职责                                                                                                        |
| ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| `control`                                       | 唯一控制 API/UI、认证、SQLite、发布器、健康协调器、Nginx Release 检查器；同一容器提供静态下载路由。                 |
| `clickhouse`                                    | 原始访问日志、分钟聚合、拨测历史；HTTP 8123 默认只映射到控制主机回环。                                              |
| `control-cert-bootstrap` / `control-cert-renew` | 使用 Cloudflare DNS-01 签发和续期控制面证书；与控制面共享操作锁。                                                   |
| `backup`                                        | 按计划创建 SQLite 在线副本和 ClickHouse 原生备份，再写入 Restic；失败状态进入消息中心并可发 SMTP。                  |
| `data/control`                                  | `control.db`、`pki/`、`letsencrypt/`、静态资源对象目录和 `nginx-artifacts/`。动态 Nginx 文件及目录由 SHA-256 派生。 |
| `data/control-tls`                              | 控制面证书和 Certbot 状态。                                                                                         |
| `backup/online-restore`                         | 在线恢复 job、维护标记、跨容器操作锁和回滚暂存。                                                                    |
| `/opt/cdn-edge`                                 | 边缘 Agent、受管 Nginx、证书、队列、日志、缓存和 systemd 单元；布局 v2 的运行文件不依赖 `/etc/nginx`。              |

详细路径和环境变量见 [CONFIGURATION.md](CONFIGURATION.md) 与 [COMPOSE_DEPLOYMENT.md](COMPOSE_DEPLOYMENT.md)。

## 5. 已实现能力

| 区域         | 当前实现                                                                                                                                                     |
| ------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| 认证与管理   | Argon2id 密码、TOTP（始终开启）、恢复码、Passkey、CSRF、会话限速、审计、品牌和持久消息中心。                                                                 |
| 节点生命周期 | 15 分钟一次性注册令牌、内部 CA/mTLS、manifest 增量轮询、机器状态 SSE、撤销、确认保护卸载和单节点/全量升级。                                                  |
| Nginx 交付   | 自编译 HTTP/2、HTTP/3、Lua、Brotli、Zstandard、stream bundle；GitHub stable 独立更新；安装器和在线升级均支持事务回滚。                                       |
| 站点发布     | 草稿与已发布快照分离；DNS-01 证书成功后才允许发布；主备节点预部署、逐节点校验、站点 HTTPS/SNI 健康和 Cloudflare DNS 滞回容灾调度。                             |
| HTTP 流量    | 静态后缀共享缓存、缓存锁/revalidate/stale、整站/URL/前缀失效、边缘本地预热、动态 gzip/Brotli/Zstandard、WebSocket、SSE、OpenAI 风格 POST 流式和 Range 透传。 |
| 回源         | HTTP/HTTPS/H2C/HTTP2、gRPC/GRPCS、主备切换、共享连接池、两层主动探测与熔断、TLS Host/SNI 分离。                                                              |
| TCP 与隧道   | stream TCP 转发、监听/上游 TLS/SNI、动态 DNS、WireGuard 源站隧道、限速和双向 TCP/UDP 性能测试。                                                              |
| 安全         | 有序 WAF 链、站点 PoW、客户端 IP 限速、nftables IPv4 封禁、能力门控和结构化安全事件。                                                                        |
| 托管资源     | 最大 32 MiB 的内容寻址对象、精确 URL 绑定、mTLS 边缘同步、大小/SHA 校验、gzip/Brotli/Zstandard sidecar。                                                     |
| 可观测性     | ClickHouse 7 天原始日志、30 天分钟聚合、三类请求 ID、回源阶段耗时、压缩统计、节点缓存/机器状态和 7 天拨测历史。                                              |
| 恢复         | Restic 日备份、短期重试、最终失败告警、离线 verify-only/切换回滚、带临时 ClickHouse 的在线恢复。                                                             |

## 6. 管理台工作区

当前 HashRouter 路由和侧栏工作区包括：

- 概览及站点分析、日志及日志详情；
- 安全、托管静态资源、缓存运维；
- 监控、节点历史、调度/智能路由；
- WireGuard 隧道及隧道详情；
- 节点列表/详情（Agent 与 Nginx 版本、候选工件、升级任务）；
- 站点列表/详情、证书；
- 设置（品牌、DNS、缓存、Cloudflare、SMTP、备份/恢复、登录与安全）。

节点页的 Nginx 区域只负责检查和批准工件；批准后仍需显式选择单节点或全部升级。

## 7. 版本与发布模型

### 控制面

- `vMAJOR.MINOR.PATCH` 标签触发稳定控制镜像发布。
- `main` 和 `sha-<commit>` 是开发镜像标签；生产部署应使用不可变 digest。
- UI 构建输出到 `internal/control/web/dist`，通过 Go `//go:embed` 编入控制二进制，因此 UI 修改需要重新构建镜像。

### Nginx

- `deploy/nginx/VERSION` 是镜像内置 bootstrap baseline，当前为 `1.30.4`。
- `.github/workflows/nginx-update.yml` 每日只检查官方 Stable 区域，发布 `nginx-vX.Y.Z` Release，不修改仓库版本文件，不发布控制镜像。
- 主控把验证后的文件保存为 `<sha256>.tar.gz`，状态写入 SQLite；候选、当前和退休工件使用内容寻址路由长期提供。
- 边缘升级同时验证 Agent SHA 和 Nginx bundle SHA；旧能力节点必须先执行一次生成部署命令。

## 8. 数据保留与恢复边界

| 数据                    | 当前策略                                                                                                                                                      |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| ClickHouse 原始访问日志 | 7 天 TTL。                                                                                                                                                    |
| ClickHouse 分钟聚合     | 30 天 TTL。                                                                                                                                                   |
| ClickHouse 拨测历史     | 7 天 TTL。                                                                                                                                                    |
| 控制面消息              | 已读消息保留三个月。                                                                                                                                          |
| 边缘访问日志            | 控制面不可达时进入本地队列，确认后上传。                                                                                                                      |
| Nginx 动态工件          | 退休文件不自动清理，需监控磁盘。                                                                                                                              |
| 托管静态资源对象        | 当前不在 Compose Restic 归档中；必须对 `$CONTROL_DATA_DIR/static-assets/objects` 做独立备份。SQLite 中的绑定元数据会恢复，但没有对象字节时 URL 无法重新下发。 |

离线恢复会替换整个 `data/control`，在线恢复会原子切换已验证的 SQLite、CA、证书、Nginx 工件和 ClickHouse。两种路径都不能凭空恢复未进入 Restic 的静态资源对象。

## 9. 关键不变量

1. 不通过 SSH 直接修改边缘 Nginx；所有站点和策略由 desired state、`nginx -t`、reload/restart 和新 worker 检查推进。
2. 不手工修改 `applied_version`、`.bundle-sha256`、Nginx 工件文件或受管 WireGuard/nftables 文件。
3. 控制面故障时保留边缘最后已知可用配置；所有节点都不健康时不向 Cloudflare 发布空记录集。
4. 发布和节点升级是显式操作；候选下载、工件批准和节点升级彼此不隐式触发。
5. 生产环境必须把控制加密密钥、Cloudflare/SMTP/Restic 凭据和静态资源对象纳入独立恢复记录。

## 10. 验证与发布门

本地最小验证：

```bash
npm --prefix frontend ci
npm --prefix frontend run check
go test ./...
go vet ./...
bash -n scripts/*.sh test/*.sh test/fake-docker
```

CI 会基于 `deploy/examples/` 中的示例文件准备 Compose 校验所需的临时环境，再执行 `docker compose config --quiet`。GitHub Actions 还执行 Playwright Chromium、Actionlint、Linux AMD64 控制镜像构建，以及 Debian 12/13 的 Nginx bundle 和安装回滚测试。修改渲染器后，部署新控制镜像并执行 `publish-all`，再按 [NGINX_APPLY_SAFETY.md](NGINX_APPLY_SAFETY.md) 验收实际节点。

## 11. 不属于仓库状态的事项

以下信息必须从部署系统和运行指标读取，不应在此文件中维护：实际节点 IP/数量、真实源站地址、Cloudflare 记录、当前故障、生产容量、备份仓库状态、当前 Nginx 候选和实时健康结果。

文档导航见 [文档索引](README.md)；配置细节见 [CONFIGURATION.md](CONFIGURATION.md)。
