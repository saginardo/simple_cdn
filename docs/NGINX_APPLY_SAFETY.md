# Nginx 配置应用与站点健康检查

边缘节点只运行 `/opt/cdn-edge/nginx/sbin/nginx`。主配置、PID、临时目录、Lua 库、日志和生成配置均位于 `/opt/cdn-edge`；`/etc/nginx` 与发行版 `nginx` 命令不参与布局 v2 的运行。

## 已确认的故障模型

`nginx -s reload` 和 `systemctl reload nginx` 只负责向 master 发送 reload 信号。命令成功不等于新 worker 已经接管；master 随后仍可能因运行态共享区冲突等原因异步拒绝候选配置。此时旧 worker、监听端口和通用健康端点可能继续正常，看起来像“发布成功”，但实际仍在使用旧虚拟主机。

因此只检查命令退出码、PID 文件或端口不足以推进 `applied_version`。

## 应用约束

1. 安装器从 Debian Nginx 或布局 v1 迁移到自管 Nginx 时执行完整 stop/start。旧 master 不会与新二进制、缓存路径或 Lua 运行时共存。
2. 已在布局 v2 的普通站点更新保持缓存根目录不变，使用 reload 避免不必要的连接中断。
3. Agent 在 reload 后最多等待 5 秒，必须看到 master 产生至少一个新 worker PID；否则恢复上一份配置、证书和 applied version。
4. 声明 UDP 443 的 HTTP/3 状态还必须在应用前通过端口归属检查，并在 reload 后确认 Nginx 同时持有预期 TCP/UDP 监听。
5. `origin_connection_v1` 的 upstream include 切换与 desired-state 应用共用串行锁。文件写入、`nginx -t`、reload 或 worker 确认任一步失败都会回滚。
6. 控制面除节点级 HTTP 探测外，还会分别直连 Edge IPv4/IPv6:443，并使用站点真实 Host、TLS SNI 和证书主机名执行站点探测。响应体必须精确标识站点；纯 TCP 站点改为探测已发布端口。
7. 节点和站点的 A/AAAA DNS 资格使用独立的连续 3 次失败摘除、5 次成功恢复滞回。备用节点与主节点一同预部署并接受健康检查，但平时不写入 DNS；主池没有健康节点时才使用健康备用节点，任一主节点恢复资格后下一轮自动切回主池。IPv4 主备池均无健康节点时保留现有 DNS 并告警。

## 何时必须 restart

以下场景不得只执行 reload：

- 从 Debian Nginx 或布局 v1 切换到 `/opt/cdn-edge/nginx`。
- Nginx 二进制、私有 LuaJIT 或静态编译模块发生变化。
- 同名共享内存区的底层定义发生运行态不兼容变更。
- 回滚上述变更并切回上一份 Nginx bundle。

普通站点增删、源站修改、证书更新以及不改变共享运行态定义的配置继续使用 reload。不要手工写 applied version、跳过 worker 检查或关闭 TLS 校验。

## Edge 验证

普通 reload 前后可用以下命令确认出现新 worker；`after` 至少应包含一个 `before` 中不存在的 PID：

```bash
nginx=/opt/cdn-edge/nginx/sbin/nginx
pid=/opt/cdn-edge/nginx/run/nginx.pid

master=$(cat "$pid")
before=$(ps --ppid "$master" -o pid=,args= | awk '/nginx: worker process/ {print $1}' | sort -n)

sudo "$nginx" -t
sudo "$nginx" -s reload
sleep 1

master=$(cat "$pid")
after=$(ps --ppid "$master" -o pid=,args= | awk '/nginx: worker process/ {print $1}' | sort -n)
printf 'before: %s\nafter:  %s\n' "$before" "$after"
sudo journalctl -u nginx.service --since '-2 minutes' --no-pager
```

部署或升级后同时核对 bundle 身份和运行依赖：

```bash
nginx=/opt/cdn-edge/nginx/sbin/nginx

sudo systemctl is-active nginx.service cdn-edge-agent.service
sudo "$nginx" -t
sudo "$nginx" -V
sudo cat /opt/cdn-edge/nginx/VERSION
sudo cat /opt/cdn-edge/nginx/.bundle-sha256
sudo ldd "$nginx" | grep -F /opt/cdn-edge/nginx/lib/libluajit-5.1.so.2
sudo "$nginx" -T 2>/dev/null | grep -F '/opt/cdn-edge/cache'
curl -fsS http://127.0.0.1/__cdn_health
sudo ss -H -ltnp '( sport = :443 )'
sudo ss -H -lunp '( sport = :443 )'
```

## 站点验证

不要只请求 Edge IP 或通用健康端点。使用 `--resolve` 将真实域名和 SNI 定向到指定节点，同时保留系统 CA 校验：

```bash
domain=cdn.example.com
edge_ip=203.0.113.20

curl --fail --silent --show-error \
  --resolve "$domain:443:$edge_ip" \
  "https://$domain/__cdn_health"
```

期望输出为 `site=<site-id>`。若返回其他站点 ID、默认证书、证书主机名错误或非 200，均视为该站点在该节点异常。再检查业务根路径：

```bash
curl --silent --show-error --output /dev/null \
  --write-out '%{http_code}\n' \
  --resolve "$domain:443:$edge_ip" \
  "https://$domain/"
```

站点开启 HTTP/3 且节点报告 `http3_v1` 时，从另一台包含 HTTP/3 支持的主机验证真实 QUIC 握手：

```bash
curl --http3-only --fail --silent --show-error \
  --resolve "$domain:443:$edge_ip" \
  --write-out '\nHTTP %{http_version}\n' \
  "https://$domain/"
```

控制面健康对账有意使用 TCP HTTPS 回退路径。本机 UDP socket 正常但外部 QUIC 失败时，应先检查主机和云安全组的 UDP 443 入站规则。

## 故障处理顺序

1. 查看 Agent 心跳中的结构化 apply 错误、`journalctl -u nginx.service` 和 `/opt/cdn-edge/logs/nginx-error.log`。
2. 核对 bundle 摘要、`nginx -t` 和 reload 后的新 worker，而不是只看 systemd 命令退出码。
3. 对每个受影响域名执行 `curl --resolve`，区分节点可达、证书/SNI、虚拟主机和源站问题。
4. 涉及二进制、模块或共享运行态冲突时，逐节点安排短维护窗口并 restart；不要重复 reload。
5. 确认新 master/worker、站点专属健康响应和业务根路径后，再继续下一节点或执行 DNS 操作。
