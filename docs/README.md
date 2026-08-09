# Documentation / 文档索引

本目录描述当前仓库实现，而不是某台生产主机的实时状态。命令中的域名、IP、端口和站点 ID 都是占位符；部署密钥、实际拓扑、故障记录和运行指标不应提交到仓库。

遇到文档与实现不一致时，按以下顺序判断：

1. `deploy/docker-compose.yaml`、`deploy/examples/*.env.example` 和实际代码定义运行行为。
2. 本目录说明这些行为的部署方式、边界和验证方法。
3. `README.md` 与 `README_CN.md` 提供项目概览和最短上手路径。

## 入门与部署

| 文档                                                            | 内容                                                        |
| --------------------------------------------------------------- | ----------------------------------------------------------- |
| [English README](../README.md) / [中文 README](../README_CN.md) | 功能概览、边界、构建、首次部署和基本工作流。                |
| [项目状态与架构](PROJECT_STATUS.md)                             | 当前模块、服务拓扑、数据所有权、发布模型和已知边界。        |
| [控制面 Compose 部署](COMPOSE_DEPLOYMENT.md)                    | 安装、镜像交付、证书、备份、离线恢复和在线恢复。            |
| [配置参考](CONFIGURATION.md)                                    | Compose、控制器、备份、在线恢复和边缘 Agent 环境变量。      |
| [边缘节点部署](EDGE_DEPLOYMENT.md)                              | `/opt/cdn-edge` 布局、安装/迁移、HTTP/3、升级、回滚和卸载。 |

## 发布与运维

| 文档                                    | 内容                                                               |
| --------------------------------------- | ------------------------------------------------------------------ |
| [Nginx 独立更新](NGINX_UPDATES.md)      | 官方 stable 检查、GitHub Release、主控下载、管理员批准和节点升级。 |
| [Nginx 应用安全](NGINX_APPLY_SAFETY.md) | reload/restart 边界、新 worker 验证、站点 SNI 健康检查。           |
| [智能路由](SMART_ROUTING.md)            | 评分与时间门控、状态所有权、通知和管理 API。                       |

## 流量、缓存与回源

| 文档                                               | 内容                                                  |
| -------------------------------------------------- | ----------------------------------------------------- |
| [压缩与缓存控制](COMPRESSION_AND_CACHE_CONTROL.md) | gzip/Brotli/Zstandard、缓存代际失效、预热和结果追踪。 |
| [透传模式](PASSTHROUGH_MODE.md)                    | HTTP(S) 整站无缓存转发及 Range 请求排障。             |
| [回源连接](ORIGIN_CONNECTIONS.md)                  | 共享连接池、两层主动探测、熔断和实时状态。            |
| [回源 TLS SNI](ORIGIN_TLS_SNI.md)                  | 连接地址、Host 和 TLS SNI 分离配置。                  |
| [WireGuard 回源](WIREGUARD_ORIGIN.md)              | 私网隧道、协议选择、限速、性能测试和卸载。            |

## 安全、资源与可观测性

| 文档                                 | 内容                                               |
| ------------------------------------ | -------------------------------------------------- |
| [安全策略](SECURITY_POLICIES.md)     | WAF、浏览器 PoW、限速、IPv4 封禁和能力门控。       |
| [托管静态资源](STATIC_ASSETS.md)     | 内容寻址对象、URL 绑定、边缘同步、备份边界和验证。 |
| [请求追踪](REQUEST_TRACING.md)       | 请求 ID、传输状态、回源字节和协议边界。            |
| [第三方声明](THIRD_PARTY_NOTICES.md) | 管理界面与自管 Nginx bundle 的许可证信息。         |

## 维护约定

- 功能文档描述稳定语义，不记录单次事故、具体生产节点或下一次临时操作。
- 默认值应以代码常量或受版本控制的环境变量示例为依据；可变版本和价格不写成长期承诺。
- 控制镜像版本使用 `vMAJOR.MINOR.PATCH` Git 标签；Nginx 使用独立的 `nginx-vX.Y.Z` Release，两者不是同一版本线。
- 修改 Compose、环境变量、公开路由、持久目录、升级流程或默认值时，应在同一变更中更新对应文档。
- 修改 Markdown 后至少执行链接检查、`git diff --check` 和涉及示例的配置/脚本校验。
