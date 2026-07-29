# WireGuard 专用回源隧道

simple_cdn 可以为一个源站和一组边缘节点建立受管 WireGuard 隧道。客户端到边缘的 HTTPS、HTTP/2 或 HTTP/3 行为不变；只有“边缘 -> 源站”这一段改走私网地址。

```text
客户端 -> Edge Nginx -> scwg<id> (10.253.x.y) == WireGuard/UDP ==> 源站 (10.253.x.1) -> 应用
                         |                                      |
                         +-- Edge Agent 管理                    +-- systemd + wg-quick 管理
```

## TLS、Caddy 与协议选择

WireGuard 已经为隧道内数据提供加密和双向密钥认证，但不会自动改变应用层协议。站点的源站 URL 决定隧道内部是否继续使用 TLS：

- `http://origin.example.com:PORT` + HTTP/1.1：隧道内使用明文 HTTP，不再需要源站证书。应用可以直接监听 WireGuard 地址或 `0.0.0.0`；如果 Caddy 只负责 TLS，可以移除这层。
- `http://origin.example.com:PORT` + H2C：隧道内使用明文 HTTP/2，适合支持 H2C 的 HTTP 服务，也不需要源站证书。
- `grpc://origin.example.com:PORT`：隧道内使用原生明文 gRPC/HTTP/2，不需要源站证书。
- `https://`、`wss://` 或 `grpcs://`：仍执行源站 TLS 验证。控制面只把连接地址改成隧道 IP，原有 Host 和 SNI 会被保留，因此适合继续使用 Caddy 或应用自身 TLS 的部署。

WireGuard 不代替应用反向代理。若 Caddy 还负责路由、认证、压缩或将请求转发给 Unix socket，可以继续保留 Caddy，只把它改为在隧道地址上提供 HTTP/H2C。

## 前置条件

- 控制面的 `CONTROL_PUBLIC_URL` 必须是外部可访问的 HTTPS URL；源站安装令牌不会通过 HTTP 下发。
- 边缘 Agent 必须上报 `wireguard_v1`。链路性能测试还要求 `wireguard_performance_v1`；重新运行新版边缘安装/升级流程会安装 `wireguard-tools` 和 `iperf3`。
- 源站使用带 systemd 的 Linux，并提供 `apt-get`、`dnf` 或 `yum`。安装脚本需要 root，安装 `wireguard-tools`、`nftables`、`jq` 和 `iperf3`。
- 源站公网入口需要允许所选 WireGuard UDP 端口。安装脚本创建的 nftables 规则只接受所选边缘公网 IPv4；云安全组仍需配置同样规则。
- 源站业务端口的公网关闭策略由运营者管理。受管脚本不会推断应用端口，也不会改动其他防火墙表；应只允许从 WireGuard 接口访问业务端口，或让应用仅监听隧道地址。
- 隧道当前仅支持 IPv4。每条隧道使用一个 RFC 1918 `/16` 到 `/28` 网段，不能与其他受管隧道重叠。

## 配置流程

1. 在 **WireGuard** 工作区添加隧道，填写源站公网 IPv4/DNS、UDP 端口、私网 CIDR，并选择边缘节点。
2. 生成源站安装命令，在源站以 root 执行。命令携带一个 256 位、15 分钟有效的令牌；控制面只保存令牌哈希，成功返回配置后令牌立即失效。
3. 等待“源站修订”和全部“边缘状态”显示已应用，并确认最近握手时间持续更新。源站和每个边缘私钥都只保存在各自主机，不上传控制面。
4. 编辑站点，在主源站或备用源站的“回源链路”中选择该隧道。选择的隧道必须覆盖站点的全部边缘节点。
5. 根据是否需要应用层 TLS 选择源站 URL 与回源 HTTP 协议，再发布站点。HTTP 可选 HTTP/1.1/H2C，原生明文 gRPC 使用 `grpc://`。发布器只替换 URL 的连接主机，保留端口、Host 和 TLS SNI。
6. 在 WireGuard 工作区运行链路测试，对比公网 TCP、隧道 TCP 和隧道 UDP。测试是有负载操作，应从较低的目标带宽和短时长开始。

源站脚本同时创建一个只用于测试的 `iperf3` 服务。公网 TCP 测试端口只允许所选边缘公网 IPv4，隧道 TCP/UDP 测试从 WireGuard 接口进入。业务应用端口是否允许从隧道访问，仍受源站现有防火墙规则约束。

## 发布与故障语义

- 站点一旦选择隧道，控制面不会在隧道异常时静默改回公网源站。源站修订、边缘公钥、边缘应用修订或节点归属不一致时，发布会明确失败。
- 已经运行的 WireGuard 接口和已发布 Nginx 配置不依赖控制面持续在线。控制面故障期间，新配置、状态汇报和性能任务会暂停，但现有业务流量继续运行。
- 修改 CIDR、端口、MTU、保活时间或节点集合会递增隧道修订。重新生成并运行源站安装命令，再等待所有 Peer 收敛后重新发布引用它的站点。
- 删除或从隧道移除仍被站点引用的节点会被控制面拒绝。先修改并发布相关站点，再执行隧道变更。

## UDP 限流与性能判断

WireGuard 只使用 UDP。部分运营商、跨境线路或云厂商可能对 UDP 限速、丢包或设置较短的 NAT 映射；`PersistentKeepalive` 默认 25 秒用于维持 NAT 状态，但不能绕过运营商限流。

判断是否适合使用隧道，应以同一边缘、同一源站、同一时间窗口的测试结果为准：

- 公网 TCP 与隧道 TCP 接近，隧道 UDP 丢包和抖动稳定：适合上线。
- 隧道 TCP 明显低于公网 TCP，同时 UDP 丢包高：优先降低 MTU、检查主机/云防火墙，再判断线路是否限制 UDP。
- 线路明确只保证 TCP：继续使用现有 HTTPS + HTTP/2/GRPCS，或在可信私网内使用 H2C。不要把业务 TCP 再套进通用 TCP 隧道作为默认方案；双重 TCP 拥塞控制和队头阻塞通常会放大丢包影响。

WireGuard 减少的是应用层 TLS 握手和源站证书运维，不会消除客户端 TLS，也不会保证每条线路都更快。长期连接本来就会复用 TLS/HTTP/2，因此实际收益通常更多来自源站暴露面收敛和运维简化。

## 排查与卸载

```bash
# 边缘或源站：接口、Peer、握手和流量
sudo wg show
ip address show scwg<隧道短 ID>

# 边缘
sudo journalctl -u cdn-edge-agent --since '-10 minutes' --no-pager

# 源站
sudo systemctl status 'wg-quick@scwg*' 'simple-cdn-origin-iperf-*'
sudo nft list tables
```

若存在分片或大包超时，先把隧道 MTU 从默认 `1420` 逐步降到 `1380` 或 `1280`，重新执行源站命令并等待边缘应用同一修订。不要直接编辑受管的 `/etc/wireguard/scwg*.conf` 或 nftables 表；下一轮配置会覆盖这些文件。

从隧道详情生成源站卸载命令。它只删除该隧道的 `wg-quick`、性能服务、密钥、状态和受管 nftables 表，不会改动应用或其他防火墙表。卸载前应先让引用该隧道的站点切换到其他可用回源并完成发布。
