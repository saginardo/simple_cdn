# simple_cdn

[English](README.md) | 简体中文

一个面向单管理员的轻量自托管 CDN：使用一台 Debian VPS 运行控制面，并由 3-10 台 Debian 12 或 13 VPS 作为边缘节点。控制器向两种发行版下发同一份项目自编译 Nginx，包含 HTTP/2、HTTP/3、stream 和 Lua 支持。Cloudflare 仅提供权威 DNS，终端用户直接连接边缘节点。

已发布版本由 `vMAJOR.MINOR.PATCH` Git 标签推导，详见仓库的[版本标签](https://github.com/saginardo/simple_cdn/tags)。

## 已实现功能

- Go 控制面，包含版本化事务式 SQLite 迁移、Argon2id 密码与 TOTP 登录、Passkey 无密码登录、一次性恢复码、持久化认证限速、TOTP 防重放、CSRF 防护、审计记录，以及紧凑的管理界面。登录失败或浏览器不支持 Passkey 时会回退到密码与 TOTP；“登录与安全”设置可更换 TOTP、添加或关闭 Passkey，而 TOTP 始终保持开启。界面提供独立的节点和站点详情页、持久化消息中心、节点机器状态、最近 24 小时缓存结果、缓存磁盘用量和带二次确认的管理流程。
- 节点优先注册：先创建待注册节点，复制一条有效期为 15 分钟的一次性引导命令；后续所有边缘请求都绑定到内部签发的 mTLS 客户端证书。
- 支持从管理界面对单个节点或全部节点执行在线升级，包括资格检查、mTLS 任务下发、所有制品的 SHA-256 校验、独立 systemd 更新器、新代理心跳就绪检查和事务式回滚。
- 按能力启用的 WAF 处理链：可按站点配置优先级和 AND 条件，检查路径、查询字符串、请求头、请求体、User-Agent 与客户端 IP；支持立即放行、仅记录、拦截和 IPv4 封禁，并内置敏感文件、恶意 PHP、路径穿越、SQL 注入、XSS 和扫描器 UA 六类规则。安全工作区同时提供按站点的浏览器 PoW、边缘本地客户端 IP 限速、持久安全事件和全节点 nftables 封禁对账。详见 [docs/SECURITY_POLICIES.md](docs/SECURITY_POLICIES.md)。
- 边缘代理以原子方式暂存版本化 Nginx 基础配置、每站点 HTTP 和 `stream` 配置片段及证书；检查本机公网 TCP 与 UDP 端口占用，校验 `/opt/cdn-edge/nginx/sbin/nginx`，重新加载健康的 Nginx，或拉起已失败/停止的 Nginx。代理会确认重新加载确实创建了新一代 worker 进程；失败时恢复最后一份正常配置和 TLS 文件，并在不阻塞心跳循环的前提下上报 Linux 主机状态和 `/opt/cdn-edge/cache` 磁盘用量。
- 可复现的 AMD64 受管 Nginx bundle：主控镜像内置 1.30.4 作为首次安装兜底，后续稳定版独立发布。每次构建都固定所有源码 SHA-256，启用加固参数、HTTP/2、HTTP/3、stream、NDK、lua-nginx-module、ngx_brotli 和 zstd-nginx-module，并私有携带 OpenResty LuaJIT/runtime。事务安装器记录并卸载 Debian Nginx 软件包，把二进制、配置、运行目录、日志、缓存和服务统一安装到 `/opt/cdn-edge`；失败时恢复准确的软件包版本和 `/etc/nginx`。
- Nginx OSS 静态资源缓存策略：每个边缘节点共享一个缓存区，默认总磁盘上限为 1 GiB。全局节点默认值可被单个节点覆盖，站点共享该节点配额。只有常见 CSS、JavaScript、字体、图片、WebAssembly 和 Web Manifest 后缀会选择缓存，其他 URI 均使用 `proxy_cache off`。携带 Authorization 或 Cookie 的请求不会读取或写入共享缓存，但这些请求头仍会原样回源。策略包括规范化缓存代际、缓存锁、重新验证、后台刷新、`STALE` 回退，以及 HTTP(S) 主备源站故障切换。HTTP(S) 站点会自动对 WebSocket Upgrade、SSE Accept、`X-CDN-Stream: 1` 和 POST 响应关闭缓存与响应缓冲；整站透传模式会对整个主机名禁用缓存和缓冲，同时转发字节范围。`grpc://` 和 `grpcs://` 源站通过客户端 HTTP/2 监听器使用原生 gRPC 代理。
- 每站点动态 gzip、Brotli 与 Zstandard 压缩默认开启，可配置不压缩 MIME 列表，默认排除 `text/event-stream`。缓存支持整站、精确 URL 与路径前缀三级失效，并可在失效发布后向每个已分配边缘下发有界预热任务。访问日志和保留 30 天的分钟聚合会记录 `Content-Encoding`、压缩响应数、估算或精确的节省字节及各编码命中数。详见 [docs/COMPRESSION_AND_CACHE_CONTROL.md](docs/COMPRESSION_AND_CACHE_CONTROL.md)。
- 托管静态资源：单文件最大 32 MiB，在控制面按内容寻址保存，经 mTLS 授权同步到相关边缘并校验大小与 SHA-256；同一资源可绑定到一个或多个站点的精确 URL。符合条件的公开文件会原子生成 `.gz`、`.br` 和 `.zst` 预压缩副本；Nginx 直接返回文件，并应用可选缓存头、MIME、ETag、GET/HEAD 限制以及 WAF、PoW 和限速处理。资源不再被引用后由边缘原子清理。详见 [docs/STATIC_ASSETS.md](docs/STATIC_ASSETS.md)。
- 按能力下发共享回源连接池：相同协议、地址、Host 和 SNI 的站点复用同一 upstream，池容量按节点 `worker_connections` 和引用权重自动分配。边缘以约 5 秒的复用服务探测持续确认应用路径，并以 32-48 秒的低频冷探测验证新建 TCP/TLS；任一层连续 2 次失败会熔断，恢复需两层分别确认。Nginx include 切换与站点发布串行、可回滚。访问日志和 30 天分钟聚合提供真实请求的回源建连、首字节、完整响应及连接复用率，节点详情实时展示当前 Nginx 回源 TCP 连接数与最新双层探测状态，且不持久化历史。详见 [docs/ORIGIN_CONNECTIONS.md](docs/ORIGIN_CONNECTIONS.md)。
- 按能力启用 WireGuard 专用回源隧道：主机私钥本地生成，控制面管理修订收敛、私网地址发布、源站一次性安装、nftables 规则，以及公网 TCP 与隧道 TCP/UDP 对照测试。站点可在加密隧道内使用 HTTP/H2C 或明文 gRPC 以取消源站 TLS 证书管理，也可保留原 Host/SNI 继续使用 HTTPS/GRPCS。详见 [docs/WIREGUARD_ORIGIN.md](docs/WIREGUARD_ORIGIN.md)。
- 按站点、按能力启用基于 UDP 443 的 HTTP/3/QUIC，默认关闭。安装器仅在 Nginx 报告 `--with-http_v3_module` 时声明 `http3_v1`；主动开启的站点在兼容节点上会增加 QUIC 监听、`Alt-Svc`、地址验证重试、UDP 冲突检查、重载后监听确认和能力自动对账，同时保留 TCP 443 上的 HTTP/1.1 与 HTTP/2 回退。IP 封禁同时覆盖 UDP 443 和 TCP 80/443。
- Nginx stream TCP 转发：客户端 TLS 终止和上游 TLS/SNI 校验可独立选择，支持动态上游 DNS 解析、按端口配置超时、原子多文件回滚，以及不监听 80/443 的纯 TCP 站点。
- Cloudflare DNS-only A 记录对账：节点可达性和站点级 HTTPS/SNI/证书健康检查都带滞回。连续 3 次探测失败时移除节点，连续 5 次成功时恢复；若所有节点均异常，则有意保持 DNS 不变。
- 经过身份验证的运行时设置：支持 60-300 秒 DNS TTL、按站点发布的 TTL 覆盖、加密保存 Cloudflare 与 SMTP 设置，以及加密保存 Restic S3/R2 备份凭据和计划。数据库覆盖优先于环境变量回退值，修改后无需重启控制器。
- 通过 Certbot Cloudflare 插件执行 DNS-01 证书签发；证书私钥在 SQLite 中保持加密，仅通过 mTLS 下发。
- ClickHouse 原始请求日志保留 7 天，分钟聚合保留 30 天。边缘为 HTTP、WebSocket 和 gRPC 主备回源统一生成并回传 `X-Request-ID`，保留客户端 ID、源站响应 ID、传输完成状态与回源字节，控制台可按任一 ID 检索；详见 [docs/REQUEST_TRACING.md](docs/REQUEST_TRACING.md)。命名 TCP 拨测目标只在 SQLite 中保存最新评分、连续失败次数和节点调度状态；每轮拨测历史通过有界异步队列进入 ClickHouse，保留 7 天，并提供多目标 1 小时至 7 天图表。控制面不可用时，边缘访问日志在本地排队。
- SMTP 告警，以及面向 SQLite、ClickHouse、控制面 TLS、内部 CA 和证书材料的加密 Restic S3 兼容每日备份，包含有界重试、状态上报、离线恢复演练和分阶段在线恢复流程。

## 明确边界

- 单管理员、仅 IPv4、Cloudflare DNS-only、单一 Cloudflare 账户；不提供租户/RBAC、GeoDNS、托管机器人信誉/CAPTCHA、流量型 DDoS 防护或控制面高可用。
- 控制面中断不会影响已经发布的边缘流量，但在恢复之前无法执行新的发布、DNS 变更和证书续期。
- “发布”有意与“创建站点”分离。站点在获得有效证书前保持暂存状态；只有所有受影响的活跃边缘节点都确认加载目标配置后，发布任务才会成功。
- 站点编辑和替换证书只更新草稿，不会直接改变已发布的站点快照。发布时会原子提升该站点的草稿和证书，只重建其新旧分配节点，并继续使用其他站点各自的已发布快照渲染配置。
- 承载 HTTPS 站点的节点通过专用默认 `server` 拒绝未知 TLS SNI，不会错误返回其他站点的证书。
- 缓存失效通过修改缓存键中的整站、精确 URL 或路径前缀代际实现。旧对象由 Nginx 的 `inactive` 和 `max_size` 自动回收，无需不受支持的 OSS 缓存清理模块，也不会同步释放磁盘空间。

将现有数据库升级到包含已发布快照的版本前，请发布或撤销每个待处理站点。旧数据行不包含重建历史已发布快照所需的上一份在线配置。若控制器发现历史发布记录缺少快照，会拒绝在这种不明确状态下重建其他站点。

## 仓库结构

```text
cmd/control           控制面可执行程序
cmd/edge-agent        边缘代理可执行程序
internal/control      API、认证、CA、发布、健康检查与 DNS 编排
frontend/             React/Vite/Tailwind/shadcn 管理控制台源码
internal/edge         节点注册、mTLS 轮询、原子应用和本地日志队列
internal/nginx        生成的 Nginx 缓存与源站配置
internal/integrations Cloudflare、Certbot 和 SMTP 适配器
internal/logstore     ClickHouse 访问日志、拨测历史和聚合存储
deploy/               Compose 与环境变量模板
scripts/              Compose 控制面辅助脚本和发布构建脚本
```

## 构建与测试

管理界面构建需要 Node.js 24 LTS 和 npm 11 或更高版本，自管 Nginx 制品构建需要 Docker。生成的 Vite 产物会嵌入 Go 控制面二进制文件。

```bash
npm --prefix frontend ci
npm --prefix frontend run check

GOCACHE=/private/tmp/simple_cdn_go_cache \
GOMODCACHE=/private/tmp/simple_cdn_gomodcache \
GOPATH=/private/tmp/simple_cdn_gopath \
go test ./...

./scripts/build-release.sh dist
./scripts/test-nginx-artifact.sh dist/cdn-nginx-linux-amd64.tar.gz
./scripts/test-edge-installer-debian.sh dist/cdn-nginx-linux-amd64.tar.gz
```

未打标签的本地构建使用 `0.0.0-dev+<commit>`（工作区有修改时追加 `.dirty`）。干净工作区恰好位于有效的 `vMAJOR.MINOR.PATCH` 标签时，直接使用该标签作为发布版本，无需修改版本文件。

浏览器冒烟测试位于 `frontend/e2e`，覆盖已登录工作区、登录页、响应式侧栏和 shadcn/Recharts 概览图表。安装 Playwright Chromium 后，运行 `npm --prefix frontend run test:e2e`。

开发管理界面时，先在 `127.0.0.1:8443` 启动 TLS 控制面，再运行 `npm --prefix frontend run dev`。Vite 会将经过身份验证的 API 请求代理到本地 TLS 端点，支持其开发证书，并保留现有哈希路由。

`dist/SHA256SUMS` 用于独立校验控制器、边缘代理和自管 Nginx bundle。控制器启动时计算 `EDGE_BINARY_PATH`，并校验 `NGINX_BUNDLE_PATH`，再把两个摘要写入注册与升级指令。控制器可在边缘控制 URL 下直接提供 `/downloads/cdn-edge-agent-linux-amd64` 和 `/downloads/cdn-nginx-linux-amd64.tar.gz`。

GitHub Actions 会为每个拉取请求执行相同的编译与校验、浏览器冒烟测试和完整 Docker 镜像构建。`main` 构建成功后发布带开发版本的 `main` 和 `sha-<commit>` 镜像；推送有效的 `vMAJOR.MINOR.PATCH` 标签会直接发布对应稳定版镜像，无需修改项目文件。工作流不会连接生产环境。私有部署自动化消费不可变 digest；控制主机只拉取镜像，不再编译源码或执行 `docker compose build`。详见 [Compose 部署文档](docs/COMPOSE_DEPLOYMENT.md#github-actions-delivery)。

独立的 `Managed Nginx stable update` 工作流每天检查一次 nginx.org 的 **Stable version** 区域。仅当官方稳定版高于 `deploy/nginx/VERSION` 时，才下载并计算官方源码哈希，构建现有 amd64 bundle，在 Debian 12/13 上完成产物与迁移测试，并在全部检查成功后发布持久的 `nginx-v<version>` GitHub Release。Release manifest 会绑定 stable 渠道、官方源码 URL 与哈希、仓库提交、bundle 大小和 SHA-256；该工作流不会重建或发布主控镜像。

## 控制面安装

Docker Compose 是控制面和 ClickHouse 的受支持部署方式。配置、SQLite、内部 CA、证书状态、ClickHouse 数据、日志和备份暂存都保存在 `/opt/cdn-platform` 下。现有公网反向代理可以独立保留；控制器仍在直连端口终止 TLS，以支持边缘 mTLS。安装、备份和恢复说明见 [docs/COMPOSE_DEPLOYMENT.md](docs/COMPOSE_DEPLOYMENT.md)。

在已安装 Docker Engine 和 Docker Compose 的全新 Debian 12 控制面 VPS 上，从可信代码检出目录执行：

```bash
sudo ./scripts/install-control-compose.sh /opt/cdn-platform
sudoedit /opt/cdn-platform/config/control.env
cd /opt/cdn-platform
sudo docker compose config --quiet
sudo docker compose pull
sudo docker compose run --rm --no-deps control keygen
```

将生成的密钥写入 `CONTROL_ENCRYPTION_KEY`。Cloudflare API Token 应仅授权系统管理的 Zone，并具备 `Zone:Read` 和 `DNS:Edit` 权限。配置 `CONTROL_TLS_DOMAIN`、`CONTROL_PUBLIC_URL`、`EDGE_CONTROL_URL` 和 `ACME_EMAIL`，然后启动服务：

```bash
sudo docker compose up -d
sudo docker compose ps
```

首次签发控制面证书和执行回滚时仍需使用 `control.env` 中的 `CLOUDFLARE_API_TOKEN`。完成管理员初始化后，可以在“设置”中保存加密的运行时覆盖。DNS 对账、站点证书任务和后续控制面证书续期都会使用该覆盖；删除覆盖后恢复环境变量值。SMTP 使用相同的整套配置覆盖和重置模型。

主控启动后立即检查 `NGINX_UPDATE_GITHUB_REPOSITORY`，之后按 `NGINX_UPDATE_CHECK_INTERVAL` 周期检查（默认 `24h`）。它只接受非草稿、非预发布的 `nginx-v<version>` Release，并要求 manifest 声明 `channel: stable`；下载后还会独立校验压缩包内容，再保存到 `$CONTROL_DATA_DIR/nginx-artifacts`。现有 `./data/control` 卷会持久化这些文件，常规备份也会包含它们。公开仓库无需 Token；私有仓库或需要更高 API 配额时设置 `NGINX_UPDATE_GITHUB_TOKEN`。

校验完成的新版本会同时出现在消息中心和“节点”页面，状态为待批准候选。管理员先明确将其设为升级目标，再选择单节点升级或“全部升级”；批准本身不会自动升级任何节点。bundle 和对应安装脚本都使用包含 SHA-256 的不可变地址，因此切换目标不会破坏在途任务。镜像内置 Nginx 继续作为首次安装兜底；主控只需部署一次这项能力，后续升级 Nginx 不再要求更新主控镜像版本。

内置 ClickHouse 配置针对 2 核、4 GiB 控制主机进行了调优：限制后台调度器，并关闭 `system.metric_log`、`system.trace_log` 等高写入量内部分析表。CDN 访问日志、分钟聚合、查询诊断、数据分片诊断、错误和异步写入诊断仍保持启用。用户级 ClickHouse 限制和性能分析开关独立安装在 `users.d` 下。

控制面 VPS 防火墙策略：

- TCP 443 仅向管理员和边缘节点开放。
- TCP 22 仅向管理来源开放。
- 除非有意使用独立日志节点，否则 ClickHouse 8123 端口只绑定 `localhost`。

当常规 HTTPS 反向代理终止管理界面流量时，不要让边缘 mTLS 经过该代理。让控制器绑定第二个直连 TLS 端口，将 `EDGE_CONTROL_URL` 指向该端口，并让 `CONTROL_PUBLIC_URL` 继续使用反向代理的标准 HTTPS 端口。将 `TRUSTED_PROXY_CIDRS` 设置为代理的回环或私网地址，使初始化限制、审计和登录限速能够安全使用其 `X-Real-IP` 请求头。

首次启动时，控制器会在 `/var/lib/cdn-platform/initialization-token`（或 `CONTROL_INITIALIZATION_TOKEN_FILE`）创建权限为 `0600` 的 32 字节一次性初始化令牌。输入本地读取的令牌和管理员密码后，控制器只返回 TOTP 密钥与恢复码，不会创建账户或消耗令牌。将密钥加入认证器、离线保存恢复码，再输入当前 TOTP；只有验证成功后才会创建管理员并删除令牌文件。该确认码已经被消费，首次登录需使用认证器的下一个验证码或一枚恢复码。

首次登录后可在“设置 → 登录与安全”中添加 Passkey 或更换 TOTP。认证因子变更只接受五分钟内完成的登录，窗口过期后需使用当前密码和已有 TOTP/恢复码重新验证；变更成功会撤销管理员的其他会话。Passkey 绑定 `CONTROL_PUBLIC_URL` 的主机名，因此该值必须与浏览器实际访问的 HTTPS 地址一致。修改主机名后，旧 RP 的凭据仍会显示并可删除，界面会分别呈现全局配置状态和当前主机名是否可用；可以关闭 Passkey 登录，但 TOTP 不能关闭并始终作为回退方式。

首次公网启动前，应尽可能将 `SETUP_ALLOW_CIDRS` 设置为管理员出口 CIDR，避免其他互联网用户抢先访问一次性初始化端点。

## 边缘节点注册

1. 在“节点”页面使用固定公网 IPv4 添加节点。
2. 将 `EDGE_BINARY_URL` 设置为对应 HTTPS 地址；控制器根据 `EDGE_BINARY_PATH` 计算代理摘要，`NGINX_BUNDLE_PATH` 提供经校验的内置兜底，管理员批准的新 Nginx 则通过主控的内容寻址地址分发。
3. 点击“注册”，在对应 Debian 12 或 13 VPS 上以 `root` 执行生成的命令。
4. 代理在本地创建私钥，使用有效期 15 分钟的一次性令牌提交 CSR，接收内部 mTLS 证书，并开始每 30 秒发送一次心跳。

同一条生成命令可以安装新边缘节点、迁移旧的分散目录布局，或升级现有 `/opt/cdn-edge` 部署。它同时校验并安装代理与自管 Nginx，在记录回滚数据后卸载 Debian Nginx，并保留节点身份、证书、已应用版本、待发送访问日志队列、读取偏移和访问日志。旧布局和已迁移节点共存期间不要发布新的站点状态。目录布局、构建、迁移检查、备份边界和回滚行为见 [docs/EDGE_DEPLOYMENT.md](docs/EDGE_DEPLOYMENT.md)。

安装器根据 `/opt/cdn-edge/nginx/sbin/nginx -V` 检测 HTTP/3，不会向不兼容节点下发 QUIC 指令。所有新建和已有站点都默认关闭 HTTP/3，需要在对应站点的流量设置中主动开启；更新后的站点设置发布后，或节点能力发生变化时，节点期望状态会自动重建。公网 HTTP/3 还要求主机防火墙和云厂商防火墙同时允许 UDP 443；阻断 UDP 不会移除 TCP 回退，但客户端可能先经历一次失败的 QUIC 尝试。

具备 `machine_status_stream_v1` 能力的代理每 5 秒采集一次主机快照，并通过复用的 mTLS HTTP/2 控制连接上报；常规 30 秒心跳仍携带最新快照作为滚动升级兼容路径。主控只在内存中保存最新值，并通过受认证的 SSE 实时更新节点详情页；页面保留 30 秒详情刷新作为断线兜底。机器状态及其中的实时回源探测结果不持久化历史数据，尚未收到快照时页面不显示对应区域。节点详情显示 Linux 发行版和版本、运行时长、1/5/15 分钟负载、逻辑 CPU 数与区间利用率、内存和根文件系统用量、默认路由接口的 RX/TX 速率，以及有样本时的回源池状态。CPU 和网络速率根据相邻 5 秒样本计算，因此代理重启后要到第二次采样才会结束短暂的采样状态。

节点上报 `online_upgrade_v1` 与 `nginx_bundle_v1` 后，“节点”页面会同时比较代理 SHA-256、自管 Nginx bundle SHA-256 与控制器制品。“全部升级”会逐节点报告已是最新、任务繁忙、离线、缺少能力或版本落后。旧代理需要最后执行一次生成的部署命令。在线升级会在停止服务前暂存并校验安装器、代理、Nginx bundle 和三个 systemd 单元，并要求新代理通过认证心跳同时报告两个目标摘要后才提交。升级期间，该节点不能参与站点发布、站点删除或节点卸载。

控制面不可用、新配置校验失败，或运行中的 Nginx 主进程异步拒绝重新加载时，代理会保留最后可用的 Nginx HTTP 和 `stream` 配置。应用状态前会检查每个目标公网 TCP 与 UDP 端口；若端口被非 Nginx 进程占用，发布任务会收到协议、端口、PID 和进程名，代理不会停止该进程。释放端口后点击“重新发布”，代理会清除 Nginx 失败状态并自动启动它。不要删除活跃边缘节点上的 `/opt/cdn-edge/data`，其中包含节点私钥、mTLS 证书、已应用版本和待发送访问日志队列。`reload` / `restart` 边界及准确的 worker 进程和站点校验命令见 [docs/NGINX_APPLY_SAFETY.md](docs/NGINX_APPLY_SAFETY.md)。

“安全”工作区在限速、静态文件和回源处理之前执行可编辑、可限定站点的 WAF 链，支持路径、URI、查询、方法、Host、User-Agent、客户端 IP、指定请求头和有界请求体条件，并提供按站点的浏览器 PoW。拦截和封禁会在回源前终止请求；IPv4 封禁由代理拥有的 `inet simple_cdn` nftables 表在 TCP 80/443 和 QUIC UDP 443 上执行并在全节点同步。旧代理必须先升级并上报对应的 WAF、PoW、限速及封禁能力。执行顺序、能力门控、防火墙归属和协议边界见 [docs/SECURITY_POLICIES.md](docs/SECURITY_POLICIES.md)。

## 边缘节点卸载

撤销授权只会使节点证书失效，不会从边缘主机删除软件或数据。退役主机应使用独立的“卸载节点”流程：

1. 暂停节点调度或撤销其授权。
2. 将节点从所有站点移除，分配替代的活跃节点，并发布每个变更站点。已禁用站点不要求替代节点。
3. 开始准备卸载。控制器只删除托管备注精确标识该节点的 Cloudflare A 记录，然后强制等待 75 秒 DNS 安全窗口。
4. 生成有效期 30 分钟的流程命令，并在边缘主机上以 `root` 执行。脚本会先显示命令绑定的节点名称、UUID 和公网 IPv4，并要求从控制终端准确输入 `UNINSTALL <节点 UUID>`；无交互终端或输入不匹配时，不会修改主机，也不会推进控制面卸载流程。对于布局 v2，确认后脚本会停止自管代理与 Nginx，删除 `/opt/cdn-edge`、systemd 链接、sysctl/logrotate 集成和旧版项目路径；不会重新安装 Debian Nginx 或重建 `/etc/nginx`。
5. 回调成功后，节点以“已卸载”状态保留用于审计。删除控制面节点记录是另一个带确认保护的操作。

如果清理提交前 Nginx 校验或重新加载失败，脚本会恢复平台配置和之前的边缘代理服务状态。“强制完成”只在主机永久不可达时修改控制面状态，不会校验或执行远程清理。

## 第一个站点

1. 添加站点的主机名、分配节点 ID、主源站和可选备源站。控制器会根据主机名自动识别 Cloudflare 区域（Zone）。站点默认继承全局 DNS TTL，也可在草稿中选择 60-300 秒覆盖；该覆盖只会在发布后生效。生成的 HTTPS 站点默认请求体上限为 128 MiB，站点表单可调整为固定的 256、512 或 1024 MiB。HTTP/HTTPS/WebSocket 代理默认上游读写空闲超时为 360 秒，可选 6、15、30 或 60 分钟。WebSocket 和 SSE 无需声明路径：WebSocket 使用 `Upgrade`，浏览器 SSE 使用 `Accept: text/event-stream`，每个 POST 都会透传 [OpenAI 风格流式响应](https://developers.openai.com/api/docs/guides/streaming-responses)，非标准客户端还可发送 `X-CDN-Stream: 1`。普通站点使用 HTTP(S) 源站；整个主机名不得使用磁盘缓存时启用透传模式；全 WebSocket 站点使用 `ws://` 或 `wss://`；全 gRPC 主机名使用 `grpc://` 或 `grpcs://`。
2. 在 Cloudflare 中保持这些主机名记录为 DNS-only。控制面只管理带有 `cdn-platform:site=<site-id>;...` 标签的记录；如果主机名已被无标签 A 记录或其他站点的 A 记录占用，操作会被拒绝。
3. 创建站点后，控制面 VPS 会立即通过权限受限的 Cloudflare Token 排队执行异步 DNS-01 任务，并加密保存结果证书。“站点”页面会轮询任务状态，刷新页面不会取消任务。同一站点同时只能存在一个活跃证书任务。
4. 执行“发布”。控制器会先检查 TLS 签发状态；证书尚未签发成功时不会创建发布任务，并提示证书正在申请。失败或缺失的签发任务会在发布预检时自动重新入队。签发成功后，控制器为每个受影响节点构建期望状态，并等待最长 90 秒，让已分配的活跃节点完成校验和应用。“站点”页面会显示逐节点冲突或超时详情；解决冲突后点击“重新发布”。
5. 等待边缘节点进入活跃状态，并连续通过 5 次节点级和站点级 HTTPS 探测。随后控制器使用站点已发布 TTL 覆盖或默认 60 秒全局值创建 DNS-only A 记录。
6. 从经过身份验证的 API 获取 `GET /api/sites/{site-id}/origin-allowlist`，将返回的 `/32` CIDR 加入源站防火墙或安全组，以防止绕过 CDN 直连源站。

对于 SMTPS、IMAPS 或其他 TCP 服务，可在同一站点添加一条或多条 TCP 规则。每条规则定义公网监听端口、上游主机/端口、监听器 TLS、上游 TLS/SNI 和超时。需要专用节点且不应监听 80/443 时选择“纯 TCP”。所有受影响节点上报自管 bundle 的 `tcp_stream_v1` 能力前，发布会被拒绝。纯 TCP 和 HTTP 站点不能共享节点，公网 80/443 端口始终保留给 HTTP 渲染器。如果 Nginx 已通过手写配置占用目标端口，代理会将其报告为非托管冲突；保留回滚副本后移除手动监听器，校验 Nginx，再从控制器发布。TCP 会话和错误日志保存在 `/opt/cdn-edge/logs`，使用项目 logrotate 策略，不会混入 HTTP 请求分析。

HTTP 边缘节点提供 `http://EDGE_IPV4/__cdn_health`。已发布 HTTP 配置还提供站点专属的 `https://SITE_DOMAIN/__cdn_health`；控制器会直接连接每个已分配边缘 IP，同时保留真实 Host、SNI 和证书校验。纯 TCP 节点改为连接每个期望的已发布 TCP 端口，不要求开放 80/443。请在节点与云厂商防火墙中开放 TCP 80/443；当 `http3_v1` 节点上至少有一个已发布站点开启 HTTP/3 时，还需开放 UDP 443。控制器有意继续用 TCP 回退路径判定健康，因此应按边缘部署文档单独验证 QUIC。源站本身应只允许返回的边缘 CIDR 入站。

如果通过 IP 访问 HTTPS/WSS/GRPCS 源站，而证书只覆盖 DNS 主机名，请分别配置源站 URL、Host 请求头和 TLS SNI。IP 连接示例、证书要求及边缘校验命令见 [docs/ORIGIN_TLS_SNI.md](docs/ORIGIN_TLS_SNI.md)。

若不希望源站应用端口继续暴露在公网，可在 **WireGuard** 工作区创建受管隧道，在源站运行一次性命令，等待源站与所有所选边缘修订收敛，再在站点的“回源链路”中选择该隧道。希望取消源站 TLS 管理时，在隧道内使用 HTTP、H2C 或 `grpc://`；仍需应用层 TLS 时使用 HTTPS/GRPCS。安装脚本不会自动关闭应用的公网端口，需在源站防火墙中单独执行。防火墙、UDP 限流、性能测试、更新和卸载语义见 [docs/WIREGUARD_ORIGIN.md](docs/WIREGUARD_ORIGIN.md)。

### Range 流量与透传模式

对于不需要视频缓存、只要求稳定转发 HTTP(S) Range 流量的整站代理，请启用透传模式并重新发布。透传模式仅支持 HTTP(S)，并会禁用 Nginx 缓存。不要在保留 `proxy_cache` 的同时只补充 `Range` / `If-Range`，这无法保证正确的回源范围语义。启用条件、限制、故障分析和 `206` 校验命令见 [docs/PASSTHROUGH_MODE.md](docs/PASSTHROUGH_MODE.md)。

证书任务使用 `CERTIFICATE_ISSUE_TIMEOUT`，默认值为 `10m`，并等待 30 秒让 Cloudflare DNS-01 TXT 记录传播。如果 Certbot 明确报告 `No TXT record found`，签发器会再等待 30 秒并重试一次；其他失败立即返回。控制面停止或重启会将进行中的任务标记为失败，而不会立即重放，以避免重复 ACME 请求；控制器恢复后再次执行“发布”会自动排队新的签发任务。经过身份验证的 `GET /api/sites/{site-id}/certificate-task` 和 `GET /api/tasks/{task-id}` API 会返回持久化任务状态和失败详情。

## 站点删除

删除站点是持久化退役流程，而不是只删除元数据。在管理界面输入准确站点名称以启动。控制器会禁用站点，只删除托管备注标识该站点的 Cloudflare A 记录，发布不包含该站点的期望状态，并等待当前分配的所有活跃边缘节点确认新配置。之后才会删除本地 Certbot lineage，以及 SQLite 中的站点元数据和加密证书。

如果活跃边缘节点失败或超时，站点会保持禁用的“删除中”状态，托管 DNS 继续撤回。修复、排空或卸载受影响节点后重试删除；系统不提供强制删除路径。审计记录和部署任务会保留，ClickHouse 访问日志继续按现有 TTL 过期。本地 Certbot 清理不会在 ACME CA 撤销已经签发的证书。

## 运行机制

```text
客户端 -> Cloudflare DNS-only A -> 边缘 Nginx -> 主源站 -> 可选备源站
                                      |
                                      +-> 磁盘缓存 / 上游故障时 STALE 回退
                                          或为指定站点启用整站透传

管理员 -> 控制面 API/UI -> Cloudflare DNS / Certbot DNS-01 / SMTP / SQLite / ClickHouse
边缘代理 --- mTLS ---> 期望状态、心跳、批量访问日志
```

使用 60 秒 TTL 时，故障窗口约为 1-2 分钟；使用 300 秒 TTL 时可能接近 6 分钟。节点需要连续 3 次、每次间隔 15 秒的探测失败后才会被移除，随后还受递归解析器缓存的有效 TTL 影响。所有已分配节点均显示异常时，系统会保留记录并发送告警，而不会主动发布空记录集。

## 备份与恢复

Compose 备份流程使用 SQLite 在线备份 API 和 ClickHouse 原生备份，再将完整恢复集写入加密 Restic 存储。流程会重试短暂故障，发布供消息中心使用的机器可读状态，并在最终失败后发送 SMTP 告警。保留策略为 7 个每日快照、4 个每周快照和 6 个每月快照。仓库凭据和每日计划可在经过身份验证的“设置”页面管理，数据库配置优先于环境变量；离线恢复凭据仍然必须单独保存。在线流量保持运行期间，“设置”页面可以下载选定快照，并在隔离的 SQLite/ClickHouse 暂存区完成校验；随后通过二次确认执行切换，仅需短暂重启控制器，并保留回滚数据。离线校验和灾难恢复流程见 [docs/COMPOSE_DEPLOYMENT.md](docs/COMPOSE_DEPLOYMENT.md)。

## 容量与后续边界

首个部署目标是所有站点合计低于 100 请求/秒，并保留 7 天原始日志。控制面 VPS 至少使用 4 vCPU、8 GiB RAM 和 160 GiB NVMe。若需要将原始日志保留超过 7 天，或长期接近 100 RPS，应先增加存储或将 ClickHouse 迁移到独立主机。
