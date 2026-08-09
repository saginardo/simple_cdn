# Nginx 独立更新与节点升级

Nginx stable 更新与控制镜像发布是两条独立版本线。控制镜像内置 `deploy/nginx/VERSION` 指定的 bundle，当前作为首次安装和灾难恢复兜底；后续官方 stable bundle 由独立 GitHub Actions 工作流构建，主控下载后作为自己的不可变静态资源分发。部署一次支持该能力的主控后，常规 Nginx 更新不再要求重新构建或替换主控容器。

## 完整流程

```text
nginx.org Stable version
        |
        v
GitHub Actions: 下载源码 -> 固定源码 SHA-256 -> 构建 amd64 bundle
        |              -> Debian 12/13 产物、迁移和回滚测试
        v
GitHub Release nginx-vX.Y.Z（bundle + manifest + checksum）
        |
        v
主控启动时和每个检查周期查询 -> 下载并验证 -> candidate
        |
        v
消息中心 / 节点页提示 -> 管理员批准 -> current
        |
        v
管理员执行单节点或全部升级 -> Edge 事务安装 -> 心跳确认或回滚
```

批准只改变后续节点升级的目标，不会自动升级任何节点。

## GitHub Actions 构建

`.github/workflows/nginx-update.yml` 默认在每天 `03:23 UTC` 运行，也支持手动触发。工作流始终检出仓库默认分支，并只解析 nginx.org 下载页的 **Stable version** 区域。

当官方 stable 不高于 `deploy/nginx/VERSION` 时不构建。发现更高版本后，工作流会：

1. 下载 `https://nginx.org/download/nginx-X.Y.Z.tar.gz` 并计算源码 SHA-256。
2. 仅在 runner 工作区临时更新 `deploy/nginx/VERSION`，不会向仓库提交版本文件。
3. 使用现有 Docker `nginx-artifact` target 和显式 `NGINX_SHA256` 构建 Linux AMD64 bundle。
4. 在 Debian 12 和 Debian 13 中验证真实 Nginx、Lua、gzip、Brotli、Zstandard、Debian 包替换和失败回滚。
5. 核对 bundle 内的 `nginx/VERSION`、`nginx/BUILD.json` 和架构。
6. 先创建草稿 Release，上传全部固定名称资产后再公开；不完整的同名 Release 会被删除并重新构建。

Release 标签格式为 `nginx-vX.Y.Z`，包含：

| 资产                                  | 用途                                                                     |
| ------------------------------------- | ------------------------------------------------------------------------ |
| `cdn-nginx-linux-amd64.tar.gz`        | 边缘安装 bundle。                                                        |
| `cdn-nginx-linux-amd64.json`          | stable 渠道、版本、架构、官方源码 URL/SHA、构建提交、大小和 bundle SHA。 |
| `cdn-nginx-linux-amd64.tar.gz.sha256` | 运维人员独立校验使用。                                                   |

该工作流不构建、不发布也不部署控制镜像。

## 主控发现与存储

更新管理器和主控运行在同一个 `control` 容器内，不需要新增 Docker 服务。默认配置：

```env
NGINX_UPDATE_ENABLED=true
NGINX_UPDATE_CHECK_INTERVAL=24h
NGINX_UPDATE_GITHUB_REPOSITORY=saginardo/simple_cdn
NGINX_UPDATE_GITHUB_TOKEN=
```

主控启动后立即检查一次，之后每隔 `NGINX_UPDATE_CHECK_INTERVAL` 检查。公开仓库通常无需 Token；私有仓库或需要更高 API 配额时配置 `NGINX_UPDATE_GITHUB_TOKEN`。`NGINX_UPDATE_GITHUB_API_URL` 只用于 GitHub Enterprise 或测试代理，并且必须是没有查询参数、用户信息和片段的 HTTPS origin。

`NGINX_UPDATE_ENABLED=false` 会同时禁用定时和管理界面的手动检查，但不会删除已经下载或批准的工件。

主控只接受：

- 标签严格匹配 `nginx-vX.Y.Z`；
- 非草稿、非预发布 Release；
- manifest schema 为 1、渠道为 `stable`、架构为 `amd64`；
- manifest 版本、标签、文件名、Release 资产大小一致；
- 官方源码 URL 严格指向对应 nginx.org HTTPS tarball；
- 源码 SHA、构建提交和 bundle SHA 格式有效；
- 下载大小和 SHA-256 与 manifest 一致；
- bundle 通过主控现有的安全归档与必需文件校验。

验证完成的文件原子写入：

```text
$CONTROL_DATA_DIR/nginx-artifacts/<bundle-sha256>.tar.gz
```

SQLite `nginx_artifacts` 目录只保存不可变元数据和状态，文件路径由 SHA-256 推导。当前状态模型为：

- `candidate`：已下载验证，等待管理员批准；
- `current`：新建节点和后续升级使用的目标；
- `retired`：曾经批准但已被新目标替换，仍保留以服务在途或历史任务。

镜像内置 bundle 不写入该目录表；没有已批准动态工件时，它就是当前兜底目标。

## 管理员操作

节点页显示当前目标、候选版本、最近检查时间和错误。发现候选后，主控还会创建去重的 `nginx_update` 消息。

管理 API 均要求管理员会话；写操作还受 CSRF 保护：

| API                                          | 行为                                            |
| -------------------------------------------- | ----------------------------------------------- |
| `GET /api/nginx/artifacts`                   | 返回检查器状态、当前目标、候选工件和工件错误。  |
| `POST /api/nginx/artifacts/check`            | 立即查询 GitHub Release；禁用检查器时返回冲突。 |
| `POST /api/nginx/artifacts/{sha256}/promote` | 将已下载候选设为当前目标。                      |

推荐发布顺序：

1. 等待自动检查，或在节点页执行手动检查。
2. 核对候选版本、Release 标签、构建提交和 SHA-256。
3. 明确批准候选；确认节点仍未自动升级。
4. 先升级一个非关键节点，检查 Agent/Nginx 版本、摘要、`nginx -t`、worker 和业务请求。
5. 使用“全部升级”处理其余符合条件的节点，并逐项查看跳过或失败原因。

旧 Agent 如果未报告 `online_upgrade_v1` 和 `nginx_bundle_v1`，需要最后运行一次节点页生成的部署/升级命令。

## 主控静态分发

主控提供内容寻址的公开 HTTPS 路由：

```text
/downloads/nginx/<sha256>/cdn-nginx-linux-amd64.tar.gz
/downloads/nginx/<sha256>/install-edge.sh
```

两类响应都使用一年 `immutable` 缓存。节点任务会同时快照版本、URL 和 SHA-256，因此批准下一个版本不会改变已排队任务引用的内容。兼容路由 `/downloads/cdn-nginx-linux-amd64.tar.gz` 和 `/install-edge.sh` 继续指向镜像内置兜底，保证旧任务和旧客户端行为不被动态目标改变。

不要把 GitHub Actions 临时 Artifact URL 直接下发给边缘节点：它有保留期和访问认证差异。GitHub Release 是持久上游，主控自己的 SHA 路由才是边缘安装的交付地址。

## 升级事务与回滚

在线升级仍使用与人工部署相同的事务安装器：

1. Agent 下载并校验安装器、Agent、新 Nginx bundle 和三个 systemd unit。
2. 独立 updater 停止服务并执行替换。
3. 新 Agent 必须通过 mTLS 心跳报告目标 Agent 和 Nginx SHA-256。
4. 包安装、配置、服务、身份、健康或 readiness 任一步失败都会恢复上一 bundle、Agent、unit 和原服务状态。

在升级任务活动期间，该节点不能参与站点发布、站点删除或节点卸载。详细主机事务见 [EDGE_DEPLOYMENT.md](EDGE_DEPLOYMENT.md)，reload/restart 和验收命令见 [NGINX_APPLY_SAFETY.md](NGINX_APPLY_SAFETY.md)。

## 备份、恢复与容量

Compose Restic 流程会归档 `nginx-artifacts`，离线恢复会随控制数据目录恢复；在线恢复也会在提交阶段原子切换该目录并保留旧目录用于回滚。因此恢复后的 SQLite 工件目录与实际文件保持一致。

动态工件当前不会自动删除。保留 retired 文件是保证旧任务 URL 稳定的设计选择，也意味着需要监控 `$CONTROL_DATA_DIR` 磁盘使用量。删除历史工件前必须确认没有任务、生成命令或外部缓存仍引用其 SHA；当前管理界面没有提供删除操作。

## 排障

- 检查器禁用：确认 `NGINX_UPDATE_ENABLED=true`，重启主控使环境变量生效。
- GitHub API 失败：检查仓库名、Token 权限、API 配额、DNS 和主控出站 HTTPS。
- 候选未出现：确认 Release 不是草稿/预发布。官方工作流应发布三个资产；主控发现候选至少需要 manifest 和 bundle。标签、manifest 和目标版本也必须匹配。
- 下载失败：查看节点页最近错误和主控日志；修复上游后执行手动检查，临时下载文件不会进入目录表或备份。
- 批准失败：确认 bundle 文件仍存在、大小匹配，且候选 SHA 没有被手工修改。
- 节点升级失败：查看逐节点任务详情及 edge updater 日志，不要手工改 `.bundle-sha256` 或 applied version。
