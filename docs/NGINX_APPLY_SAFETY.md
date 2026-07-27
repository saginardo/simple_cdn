# Nginx 配置应用、迁移与站点健康检查

本文说明 Nginx reload 的异步失败边界、旧 Edge 目录迁移为何必须冷重启，以及如何验证实际接管流量的 worker 和站点 HTTPS 虚拟主机。

## 已确认的故障模型

旧布局迁移会把同一个 `cdn_cache` 共享缓存区的磁盘路径从 `/var/cache/cdn-platform` 改为 `/opt/cdn-edge/cache`。`nginx -t` 只校验磁盘上的候选配置，不会把它和正在运行的 master 内存状态比较，因此可以通过。

`nginx -s reload` 和 `systemctl reload nginx` 只负责向 master 发送 reload 信号。命令成功不等于新 worker 已启动；master 随后仍可能在错误日志中异步拒绝配置，例如：

```text
cache "cdn_cache" uses the "/opt/cdn-edge/cache" cache path while previously it used the "/var/cache/cdn-platform" cache path
```

此时旧 worker、80/443 监听和通用 `http://EDGE_IP/__cdn_health` 都保持正常，但实际仍在使用旧虚拟主机配置。仅检查命令退出码、端口和通用健康端点会产生假成功，并可能让控制面错误地推进 `applied_version`。

## 修复后的约束

1. 旧布局迁移改变 `cdn_cache` 路径时，安装脚本使用 `systemctl restart nginx.service`。迁移失败并恢复旧配置时同样冷重启，确保恢复后的 master 与磁盘配置一致。
2. 已经位于 `/opt/cdn-edge` 的普通升级保持缓存路径不变，仍使用 reload，避免不必要的连接中断。首次接入节点容量管理时，安装器会原子加入带标记的主配置和 `events` include，并在同一事务内验证；失败会恢复原始主配置。
3. Edge Agent reload 后最多等待 5 秒，必须看到 master 产生至少一个新的 Nginx worker PID。没有新 worker 就把应用标记为失败，恢复上一份配置和证书，并且不写入新的 applied version。对于 `http3_v1` 状态，Agent 还会在应用前检查 UDP 443 占用者，并在 reload 后确认 Nginx 同时持有 TCP 与 UDP 监听；任一检查失败都走同一回滚路径。
4. 控制面保留节点级 HTTP 探测，同时对已包含新能力的每个站点配置执行直连 Edge IP:443 的 HTTPS 探测。请求 URL、HTTP Host、TLS SNI 和证书主机名校验都使用站点真实域名，响应体必须精确标识该站点。
5. 站点与节点的 DNS 资格分别使用 3 次失败摘除、5 次成功恢复的滞回。多节点站点只摘除失败节点；如果所有已分配节点都不合格，系统保留现有 DNS 并发送告警，避免主动发布空记录集。
6. 滚动发布期间，尚未包含站点探针端点的旧 desired state 暂时只使用节点级健康结果。执行 `publish-all` 并由 Edge 应用新配置后，控制面自动切换到站点级判断。
7. 节点上的所有缓存站点共用 `cdn_cache` zone、`/opt/cdn-edge/cache` 路径和一份节点总配额。由旧的按站点 zone/目录切回共享布局时，zone 名称不同，因此普通 reload 不会触发同名 zone 路径冲突；新 worker 接管后，Agent 清理退出配置的旧站点缓存目录。
8. `origin_connection_v1` 节点的 upstream server 位于 `/opt/cdn-edge/config/nginx/origin-pools/*.conf`。主动探测的熔断切换与 desired-state 应用共用串行锁；include、边缘持久化状态或 reload 任一步失败都会恢复旧文件和旧 worker 配置。不要手工修改池文件或并行执行 reload，详见 [ORIGIN_CONNECTIONS.md](ORIGIN_CONNECTIONS.md)。

## 何时必须 restart

以下已确认场景不得只执行 reload：

- 同一个 `keys_zone` 名称对应的 `proxy_cache_path` 从旧目录切换到新目录。
- 回滚上述目录迁移并切回旧缓存路径。

普通站点增删、源站修改、证书更新和不改变运行中共享缓存区定义的 Nginx 配置继续使用 reload。不要为了规避 Agent 的“reload 未接管”错误而手工写 applied version、跳过 worker 检查或关闭 TLS 校验；应先读取 Nginx 错误日志并判断是否需要计划内 restart。

## Edge 验证

普通 reload 前后可以用以下命令确认出现新 worker。`after` 至少应包含一个 `before` 中不存在的 PID：

```bash
master=$(cat /run/nginx.pid)
before=$(ps --ppid "$master" -o pid=,args= | awk '/nginx: worker process/ {print $1}' | sort -n)

sudo nginx -t
sudo nginx -s reload
sleep 1

master=$(cat /run/nginx.pid)
after=$(ps --ppid "$master" -o pid=,args= | awk '/nginx: worker process/ {print $1}' | sort -n)
printf 'before: %s\nafter:  %s\n' "$before" "$after"
sudo journalctl -u nginx.service --since '-2 minutes' --no-pager
```

旧目录迁移完成后还应确认 master PID 已改变、服务正常且配置中不再引用旧路径：

```bash
sudo systemctl is-active nginx.service cdn-edge-agent.service
sudo nginx -t
sudo nginx -T 2>/dev/null | grep -F '/opt/cdn-edge/cache'
test -z "$(sudo nginx -T 2>/dev/null | grep -F '/var/cache/cdn-platform')"
curl -fsS http://127.0.0.1/__cdn_health
sudo ss -H -ltnp '( sport = :443 )'
sudo ss -H -lunp '( sport = :443 )'
```

## 站点验证

不要只请求 Edge IP 或通用健康端点。使用 `--resolve` 可以在不修改公网 DNS 的情况下，把真实域名和 SNI 定向到指定节点，同时保留系统 CA 的证书校验：

```bash
domain=cdn.example.com
edge_ip=203.0.113.20

curl --fail --silent --show-error \
  --resolve "$domain:443:$edge_ip" \
  "https://$domain/__cdn_health"
```

期望输出为 `site=<site-id>`。如果返回另一个站点 ID、默认证书、证书主机名错误或非 200，均视为该站点在该节点异常。再检查真实根路径，确认业务响应而不只是探针：

```bash
curl --silent --show-error --output /dev/null \
  --write-out '%{http_code}\n' \
  --resolve "$domain:443:$edge_ip" \
  "https://$domain/"
```

若节点报告 `http3_v1`，还应从另一台主机验证真实 QUIC 握手。该检查需要 curl 自身包含 HTTP/3 支持；输出必须为 `HTTP 3`：

```bash
curl --http3-only --fail --silent --show-error \
  --resolve "$domain:443:$edge_ip" \
  --write-out '\nHTTP %{http_version}\n' \
  "https://$domain/"
```

控制面健康对账有意继续使用 TCP HTTPS 回退路径，因此 TCP 健康不能证明 UDP 443 已通过云厂商防火墙。若本机 `ss -lunp` 显示 Nginx，但外部 QUIC 请求失败，应先检查主机与云安全组的 UDP 443 入站规则。

控制面可用以下查询核对站点级滞回状态；`last_error` 会保留最近一次域名、TLS 或响应体错误：

```bash
sudo sqlite3 -header -column /opt/cdn-platform/data/control/control.db \
  'select site_id,node_id,consecutive_failures,consecutive_successes,dns_eligible,last_error from site_node_health order by site_id,node_id;'
```

## 故障处理顺序

1. 先查看 Agent 心跳中的结构化 apply 错误和 `journalctl -u nginx.service`，确认 reload 是否产生新 worker。
2. 对每个受影响域名执行 `curl --resolve`，区分节点可达、证书/SNI、虚拟主机和源站业务问题。
3. 如果日志命中缓存路径冲突，逐节点安排短维护窗口并 restart Nginx；不要重复 reload。
4. 确认 `nginx -t`、新 master/worker、站点专属健康响应和业务根路径后，再继续下一节点或执行 DNS 操作。
