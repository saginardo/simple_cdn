# 配置参考

Docker Compose 部署使用三类环境文件：

```text
/opt/cdn-platform/.env
/opt/cdn-platform/config/control.env
/opt/cdn-platform/config/backup.env
```

受版本控制的模板分别是 `deploy/docker-compose.yaml` 和 `deploy/examples/*.env.example`。路径表中的容器路径不是宿主机路径；Compose 将宿主机 `/opt/cdn-platform/data/control` 挂载为容器内 `/var/lib/cdn-platform`。

## 配置优先级

- Cloudflare、SMTP 和 S3/Restic 配置可以在管理界面保存完整覆盖；SQLite 覆盖优先于环境变量，重置后恢复环境值。
- 首次控制证书签发、裸机灾难恢复和数据库尚不可用的阶段仍依赖环境文件，因此必须保留可用的离线恢复凭据。
- Nginx 更新源、控制监听、TLS 路径、ClickHouse 连接和制品路径只读取环境变量；修改后需要重建或重启相应容器。
- `control.env`、`backup.env`、`restic-password`、初始化令牌和任何运行时生成的凭据都不得提交到 Git。

## Compose `.env`

| 变量                | 默认/安装器值                       | 说明                                                                              |
| ------------------- | ----------------------------------- | --------------------------------------------------------------------------------- |
| `CDN_CONTROL_IMAGE` | `ghcr.io/saginardo/simple_cdn:main` | 控制、证书和备份服务共用的镜像。生产应固定 `sha-<commit>` 或 `@sha256:<digest>`。 |
| `CDN_DEPLOY_DIR`    | `./app`                             | Compose 支持脚本和 ClickHouse 配置的宿主机目录。                                  |

`scripts/install-control-compose.sh` 会写入这两个值；`scripts/deploy-control-compose.sh` 只接受项目 GHCR 镜像标签或 digest。

## 控制器核心配置

| 变量                                | 默认值                                         | 说明                                                                                                                                             |
| ----------------------------------- | ---------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `CONTROL_ENCRYPTION_KEY`            | 无，必填                                       | 加密 SQLite 中的证书和运行时秘密。使用 `docker compose run --rm --no-deps control keygen` 生成并离线备份。更换已有部署的密钥会使加密数据不可读。 |
| `CONTROL_DATA_DIR`                  | `/var/lib/cdn-platform`                        | SQLite、内部 CA、站点 Certbot 状态、静态对象和动态 Nginx 工件根目录。                                                                            |
| `CONTROL_INITIALIZATION_TOKEN_FILE` | `$CONTROL_DATA_DIR/initialization-token`       | 首次管理员初始化令牌；成功完成 TOTP 设置后删除。                                                                                                 |
| `CONTROL_LISTEN`                    | 二进制默认 `:443`；Compose 示例 `:8443`        | 控制器直接 TLS 监听地址。受版本控制的 Compose 健康检查固定访问容器 host network 的 `127.0.0.1:8443`，因此标准 Compose 部署应保持示例值。         |
| `CONTROL_PUBLIC_URL`                | 无有效生产默认值                               | 浏览器访问的外部 HTTPS 根 URL。Passkey RP ID 和源站安装命令依赖其主机名。                                                                        |
| `EDGE_CONTROL_URL`                  | `CONTROL_PUBLIC_URL`                           | Edge Agent 直接进行 mTLS、下载制品和回调的 HTTPS 根 URL。存在管理反向代理时通常使用控制器直连端口，例如 `https://control.example.com:8443`。     |
| `CONTROL_TLS_DOMAIN`                | 无                                             | 控制证书域名；Compose Certbot 服务要求填写。                                                                                                     |
| `CONTROL_TLS_CERT`                  | 无，控制进程必填                               | 控制器证书完整链容器路径，示例为 `/var/lib/cdn-control-tls/live/<domain>/fullchain.pem`。                                                        |
| `CONTROL_TLS_KEY`                   | 无，控制进程必填                               | 对应私钥容器路径。控制器会热加载续期后的证书。                                                                                                   |
| `CONTROL_TLS_DIR`                   | `/var/lib/cdn-control-tls`                     | 在线恢复切换控制 TLS 时使用的容器目录。                                                                                                          |
| `SETUP_ALLOW_CIDRS`                 | 空                                             | 允许首次初始化的客户端 CIDR，逗号分隔；公网启动前建议限制为管理员出口。                                                                          |
| `TRUSTED_PROXY_CIDRS`               | 空                                             | 可以提供 `X-Real-IP` 的反向代理 CIDR。不要填写不受控制的公网地址段。                                                                             |
| `BACKUP_STATUS_FILE`                | `/var/lib/cdn-platform-operations/backup.json` | 主控读取备份调度状态的位置；Compose 已显式设置并只读挂载。                                                                                       |

管理入口和边缘 mTLS 可以共用同一个 TLS listener，但反向代理不得终止边缘客户端证书。推荐拓扑见 [COMPOSE_DEPLOYMENT.md](COMPOSE_DEPLOYMENT.md)。

## Cloudflare 与证书

| 变量                        | 默认值                   | 说明                                                                                             |
| --------------------------- | ------------------------ | ------------------------------------------------------------------------------------------------ |
| `CLOUDFLARE_API_TOKEN`      | 空                       | DNS-only A 记录和 DNS-01 的环境回退 Token。至少限制到受管 Zone，并授予 `Zone:Read`、`DNS:Edit`。 |
| `ACME_EMAIL`                | 空，Compose Certbot 必填 | 控制证书和站点证书的 ACME 联系邮箱。                                                             |
| `CERTIFICATE_ISSUE_TIMEOUT` | `10m`                    | 单个站点 Certbot DNS-01 任务的正 Go duration。                                                   |

管理界面保存的 Cloudflare Token 会覆盖环境值，但首次控制证书 bootstrap 仍需要环境 Token。

## Edge Agent 与内置制品

| 变量                | 默认值                                                     | 说明                                                                                                                       |
| ------------------- | ---------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| `EDGE_BINARY_PATH`  | 无                                                         | 主控容器中 Agent 二进制路径；镜像使用 `/usr/local/lib/cdn-platform/cdn-edge-agent-linux-amd64`。启动时读取并计算 SHA-256。 |
| `EDGE_BINARY_URL`   | 无                                                         | 边缘下载 Agent 的 HTTPS URL，标准路由为 `$EDGE_CONTROL_URL/downloads/cdn-edge-agent-linux-amd64`。                         |
| `NGINX_BUNDLE_PATH` | `/usr/local/lib/cdn-platform/cdn-nginx-linux-amd64.tar.gz` | 镜像内置 bootstrap bundle；启动时执行结构、版本、大小和 SHA 校验。                                                         |
| `NGINX_BUNDLE_URL`  | `$EDGE_CONTROL_URL/downloads/cdn-nginx-linux-amd64.tar.gz` | 内置兼容 bundle URL。动态更新使用包含 SHA 的独立 URL。                                                                     |

`EDGE_BINARY_PATH` 和 `NGINX_BUNDLE_PATH` 是容器内路径，不应指向 GitHub Actions 临时 Artifact。

## Nginx stable 更新

| 变量                             | 默认值                   | 说明                                                                      |
| -------------------------------- | ------------------------ | ------------------------------------------------------------------------- |
| `NGINX_UPDATE_ENABLED`           | `true`                   | 启用启动检查、周期检查和手动检查。接受 `true/false`、`1/0` 等 Go 布尔值。 |
| `NGINX_UPDATE_CHECK_INTERVAL`    | `24h`                    | 两次检查之间的正 Go duration。主控启动时仍会立即检查。                    |
| `NGINX_UPDATE_GITHUB_REPOSITORY` | `saginardo/simple_cdn`   | 提供 `nginx-vX.Y.Z` Release 的 `owner/repository`。                       |
| `NGINX_UPDATE_GITHUB_TOKEN`      | 空                       | 私有 Release 或提高 API 配额时使用；不会下发给边缘。                      |
| `NGINX_UPDATE_GITHUB_API_URL`    | `https://api.github.com` | GitHub API HTTPS origin，主要用于 GitHub Enterprise 或测试。              |

下载目录固定为 `$CONTROL_DATA_DIR/nginx-artifacts`。完整信任链、状态和升级流程见 [NGINX_UPDATES.md](NGINX_UPDATES.md)。

## ClickHouse

| 变量                  | 默认值                  | 说明                                                                                            |
| --------------------- | ----------------------- | ----------------------------------------------------------------------------------------------- |
| `CLICKHOUSE_URL`      | `http://127.0.0.1:8123` | HTTP API；标准 Compose 通过 host network 连接本机映射端口。                                     |
| `CLICKHOUSE_DATABASE` | `simple_cdn`            | 日志、分钟聚合和拨测历史数据库。旧值 `cdn_platform` 会迁移到当前名称。                          |
| `CLICKHOUSE_USER`     | 空                      | 可选 HTTP 用户。                                                                                |
| `CLICKHOUSE_PASSWORD` | 空                      | 可选 HTTP 密码。                                                                                |
| `CLICKHOUSE_DISABLED` | `0`                     | 仅值 `1` 禁用日志/历史和在线恢复初始化，主要用于开发测试；标准 Compose 仍定义 ClickHouse 依赖。 |
| `CLICKHOUSE_HOST_GID` | `101`                   | 在线恢复暂存目录需要共享的宿主机 ClickHouse GID。                                               |

Compose 将 ClickHouse 8123 仅映射到 `127.0.0.1`，不应直接暴露到公网。

## SMTP

| 变量            | 默认值                           | 说明                                       |
| --------------- | -------------------------------- | ------------------------------------------ |
| `SMTP_HOST`     | 空                               | SMTP 主机。                                |
| `SMTP_PORT`     | `587`（STARTTLS）或 `465`（TLS） | 1-65535。                                  |
| `SMTP_SECURITY` | `starttls`                       | 只接受 `starttls` 或 `tls`。               |
| `SMTP_USER`     | 空                               | 登录用户名。                               |
| `SMTP_PASSWORD` | 空                               | 环境回退密码。                             |
| `SMTP_FROM`     | 空                               | 发件地址。                                 |
| `SMTP_TO`       | 空                               | 逗号分隔收件人；为空时环境 SMTP 配置禁用。 |

管理界面还保存通知类别选择；数据库中的完整 SMTP profile 优先于上表环境值。

## 备份与 Restic

`config/backup.env` 是首次初始化和灾难恢复回退。管理界面保存的完整备份 profile 在正常运行时优先。

| 变量                          | 默认值                               | 说明                                                          |
| ----------------------------- | ------------------------------------ | ------------------------------------------------------------- |
| `BACKUP_TIME`                 | `03:25`                              | `Asia/Shanghai` 每日计划时间。                                |
| `BACKUP_RANDOM_DELAY_SECONDS` | `1200`                               | 计划时间后的 0 到该值随机延迟。                               |
| `BACKUP_MAX_ATTEMPTS`         | `3`                                  | 单次调度的最大尝试次数。                                      |
| `BACKUP_RETRY_DELAYS_SECONDS` | `30,120`                             | 相邻尝试间隔列表。                                            |
| `BACKUP_STAGING_DIR`          | `/backup/staging`                    | SQLite/ClickHouse 备份暂存目录。                              |
| `RESTIC_REPOSITORY`           | 无                                   | S3 兼容 Restic repository URL。                               |
| `RESTIC_PASSWORD_FILE`        | `/deployment/config/restic-password` | 环境回退密码文件。必须另存离线副本。                          |
| `RESTIC_PASSWORD`             | 空                                   | 命令级直接密码；存在时优先于密码文件。标准 Compose 使用文件。 |
| `AWS_ACCESS_KEY_ID`           | 空                                   | S3 访问密钥 ID。                                              |
| `AWS_SECRET_ACCESS_KEY`       | 空                                   | S3 Secret。                                                   |
| `AWS_DEFAULT_REGION`          | `us-east-1`                          | S3 区域。                                                     |
| `CONTROL_TLS_DIR`             | `/var/lib/cdn-control-tls`           | 备份控制证书目录。                                            |

Restic 快照当前包含 SQLite 在线副本、内部 CA、站点 Certbot 状态、已下载 Nginx 工件、控制 TLS、ClickHouse 原生备份、Compose 定义和环境文件。它当前不包含 `$CONTROL_DATA_DIR/static-assets/objects`；托管静态资源对象字节必须单独备份，详见 [STATIC_ASSETS.md](STATIC_ASSETS.md#backup-and-restore)。

## 在线与离线恢复高级配置

| 变量                                           | 默认值                          | 说明                                                            |
| ---------------------------------------------- | ------------------------------- | --------------------------------------------------------------- |
| `ONLINE_RESTORE_ROOT`                          | `/var/lib/cdn-platform-restore` | 在线恢复 job、操作锁、维护标记和暂存根目录。                    |
| `ONLINE_RESTORE_READY_TIMEOUT`                 | `2m`                            | 启动阶段等待 ClickHouse 可用。                                  |
| `ONLINE_RESTORE_APPLY_TIMEOUT`                 | `30m`                           | 已提交恢复在主控启动时切换的总时限。                            |
| `ONLINE_RESTORE_PREPARE_TIMEOUT`               | `2h`                            | 下载、验证和临时 ClickHouse 恢复总时限。                        |
| `ONLINE_RESTORE_LIST_TIMEOUT`                  | `1m`                            | 列出 Restic 快照的时限。                                        |
| `ONLINE_RESTORE_QUIESCE_TIMEOUT`               | `2m`                            | 提交前等待备份和证书写操作退出。                                |
| `SIMPLE_CDN_ROOT`                              | `/opt/cdn-platform`             | 离线恢复脚本的部署根目录。旧名 `CDN_PLATFORM_ROOT` 仍作为回退。 |
| `ALLOW_NONEMPTY_RESTORE`                       | `0`                             | 离线恢复覆盖已有 SQLite 前必须显式设为 `1`。                    |
| `RESTORE_CLICKHOUSE_READY_TIMEOUT_SECONDS`     | `120`                           | 离线恢复等待 ClickHouse 秒数。                                  |
| `RESTORE_CLICKHOUSE_OPERATION_TIMEOUT_SECONDS` | `1800`                          | 单个 ClickHouse 恢复/校验操作秒数。                             |
| `RESTORE_DOWNLOAD_TIMEOUT_SECONDS`             | `3600`                          | Restic 下载秒数。                                               |

恢复流程和失败闭锁语义见 [COMPOSE_DEPLOYMENT.md](COMPOSE_DEPLOYMENT.md#restore)。

## Edge `edge.env`

边缘文件由主控安装器生成，通常不应手工维护。`ENROLLMENT_TOKEN` 只存在于首次注册前，成功后会清空。

| 变量                       | 默认值                                                | 说明                                                            |
| -------------------------- | ----------------------------------------------------- | --------------------------------------------------------------- |
| `CONTROL_URL`              | 无，必填                                              | Edge mTLS 控制 URL。                                            |
| `ENROLLMENT_TOKEN`         | 空                                                    | 15 分钟一次性注册令牌。已有完整身份的升级命令不携带新令牌。     |
| `EDGE_POLL_SECONDS`        | `30`                                                  | 心跳/manifest 轮询间隔，允许 5-300 秒。                         |
| `EDGE_STATE_DIR`           | `/opt/cdn-edge/data`                                  | 节点身份、队列、应用版本和任务状态。                            |
| `EDGE_CERT_DIR`            | `/opt/cdn-edge/config/certs`                          | 站点 TLS 文件。                                                 |
| `EDGE_STATIC_ASSET_DIR`    | `/opt/cdn-edge/static/objects`                        | 已授权静态对象及预压缩副本。                                    |
| `EDGE_ACCESS_LOG`          | `/opt/cdn-edge/logs/access.json`                      | HTTP 访问日志输入。                                             |
| `EDGE_SECURITY_LOG`        | `/opt/cdn-edge/logs/security.json`                    | WAF/限速/封禁事件输入。                                         |
| `NGINX_BINARY_PATH`        | `/opt/cdn-edge/nginx/sbin/nginx`                      | 自管 Nginx。                                                    |
| `NGINX_CONFIG_PATH`        | `/opt/cdn-edge/config/nginx/cdn-platform.conf`        | HTTP 稳定入口。                                                 |
| `NGINX_STREAM_CONFIG_PATH` | `/opt/cdn-edge/config/nginx/cdn-platform-stream.conf` | stream 稳定入口。                                               |
| `NGINX_MAIN_CONFIG_PATH`   | `/opt/cdn-edge/config/nginx/cdn-platform-main.conf`   | 主配置 include。                                                |
| `NGINX_EVENTS_CONFIG_PATH` | `/opt/cdn-edge/config/nginx/cdn-platform-events.conf` | events include。                                                |
| `NGINX_PID_PATH`           | `/opt/cdn-edge/nginx/run/nginx.pid`                   | master PID。                                                    |
| `NGINX_STATUS_SOCKET_PATH` | `/opt/cdn-edge/nginx/run/status.sock`                 | Unix-only `stub_status` socket。                                |
| `NGINX_VERSION_PATH`       | `/opt/cdn-edge/nginx/VERSION`                         | 已安装 Nginx 版本。                                             |
| `NGINX_SHA256_PATH`        | `/opt/cdn-edge/nginx/.bundle-sha256`                  | 已安装 bundle SHA-256。                                         |
| `EDGE_CAPABILITIES`        | 安装器生成                                            | 逗号分隔能力；由已安装 Agent/Nginx 检测结果决定，不应手工伪造。 |

完整目录、所有权和事务边界见 [EDGE_DEPLOYMENT.md](EDGE_DEPLOYMENT.md)。
