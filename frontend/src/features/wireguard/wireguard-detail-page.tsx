import { useQuery } from "@tanstack/react-query";
import { ArrowLeft, RefreshCw } from "lucide-react";
import { Link, useParams } from "react-router";

import {
  EmptyState,
  PageBody,
  PageError,
  PageHeader,
  PageLoading,
  Panel,
} from "@/components/page";
import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { api } from "@/lib/api";
import {
  formatBytes,
  formatDateTime,
  formatNumber,
  shortHash,
} from "@/lib/format";
import { t, useI18n } from "@/lib/i18n";
import type {
  WireGuardOriginService,
  WireGuardOriginServiceStatus,
  WireGuardPeer,
  WireGuardTunnel,
  WireGuardTunnelDetail,
} from "@/lib/types";

export function WireGuardDetailPage() {
  useI18n();
  const { tunnelId = "" } = useParams();
  const detail = useQuery({
    queryKey: ["wireguard-tunnel", tunnelId],
    queryFn: () =>
      api<WireGuardTunnelDetail>(
        `/api/wireguard/tunnels/${encodeURIComponent(tunnelId)}`,
      ),
    refetchInterval: 5_000,
  });
  const tunnel = detail.data?.tunnel;

  return (
    <>
      <PageHeader
        title={tunnel?.name ?? t("隧道详情")}
        description={
          tunnel
            ? `${tunnel.endpoint_host}:${tunnel.listen_port} · ${tunnel.address_cidr} · r${tunnel.revision}`
            : t("隧道状态与回源服务")
        }
        actions={
          <>
            <Button asChild variant="outline">
              <Link to="/wireguard">
                <ArrowLeft />
                {t("返回隧道")}
              </Link>
            </Button>
            <Button
              variant="outline"
              size="icon"
              aria-label={t("刷新隧道详情")}
              disabled={detail.isFetching}
              onClick={() => void detail.refetch()}
            >
              <RefreshCw
                className={detail.isFetching ? "animate-spin" : undefined}
              />
            </Button>
          </>
        }
      />
      <PageBody>
        {detail.isLoading ? <PageLoading /> : null}
        {detail.error ? (
          <PageError title={t("隧道加载失败")} error={detail.error} />
        ) : null}
        {tunnel && detail.data ? (
          <>
            <TunnelFacts tunnel={tunnel} />
            <OriginServices services={detail.data.origin_services} />
            <TunnelPeers tunnel={tunnel} />
          </>
        ) : null}
      </PageBody>
    </>
  );
}

function TunnelFacts({ tunnel }: { tunnel: WireGuardTunnel }) {
  return (
    <dl className="grid gap-x-6 gap-y-4 border-y py-4 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-5">
      <Fact
        label={t("公网端点")}
        value={`${tunnel.endpoint_host}:${tunnel.listen_port}`}
      />
      <Fact label={t("源站隧道 IP")} value={tunnel.origin_address} />
      <Fact label={t("隧道 CIDR")} value={tunnel.address_cidr} />
      <Fact
        label={t("源站修订")}
        value={`r${tunnel.origin_configured_revision} / r${tunnel.revision}`}
      />
      <Fact label={t("源站公钥")} value={shortHash(tunnel.origin_public_key)} />
      <Fact label={t("性能端口")} value={String(tunnel.performance_port)} />
      <Fact
        label={t("源站出口上限")}
        value={formatEgressLimit(tunnel.origin_egress_limit_mbps)}
      />
      <Fact label="MTU" value={String(tunnel.mtu)} />
      <Fact
        label={t("保活间隔")}
        value={`${tunnel.persistent_keepalive_seconds}s`}
      />
      <Fact
        label={t("源站应用时间")}
        value={formatDateTime(tunnel.origin_configured_at)}
      />
    </dl>
  );
}

function OriginServices({ services }: { services: WireGuardOriginService[] }) {
  return (
    <section className="space-y-3" aria-labelledby="wireguard-origin-services">
      <h2 id="wireguard-origin-services" className="text-base font-semibold">
        {t("回源服务")}
      </h2>
      {services.length ? (
        <Panel>
          <Table className="min-w-[1080px]">
            <TableHeader>
              <TableRow>
                <TableHead className="pl-5">{t("状态")}</TableHead>
                <TableHead>{t("端口")}</TableHead>
                <TableHead>{t("协议")}</TableHead>
                <TableHead>{t("HTTP 版本")}</TableHead>
                <TableHead>{t("关联站点")}</TableHead>
                <TableHead className="text-right">{t("边缘可达")}</TableHead>
                <TableHead className="pr-5">{t("最后上报")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {services.map((service) => {
                const state = originServiceState(service.status);
                return (
                  <TableRow
                    key={`${service.port}:${service.scheme}:${service.http_version ?? "grpc"}`}
                  >
                    <TableCell className="pl-5">
                      <StatusBadge status={state.status} label={state.label} />
                    </TableCell>
                    <TableCell className="font-mono font-medium tabular-nums">
                      {service.port}
                    </TableCell>
                    <TableCell>{formatOriginScheme(service.scheme)}</TableCell>
                    <TableCell>{formatOriginHTTPVersion(service)}</TableCell>
                    <TableCell className="max-w-md whitespace-normal">
                      <div className="space-y-1.5">
                        {service.sites.map((site) => (
                          <div key={`${site.site_id}:${site.role}`}>
                            <div className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5 text-xs">
                              <Link
                                to={`/sites/${encodeURIComponent(site.site_id)}`}
                                className="font-medium hover:underline"
                              >
                                {site.site_name}
                              </Link>
                              <span className="text-muted-foreground">
                                {site.role === "primary"
                                  ? t("主源站")
                                  : t("备用源站")}
                              </span>
                              {!site.published ? (
                                <span className="text-warning">
                                  {t("待发布")}
                                </span>
                              ) : null}
                            </div>
                            {site.domains.length ? (
                              <div className="mt-0.5 max-w-sm truncate font-mono text-xs text-muted-foreground">
                                {site.domains.join(", ")}
                              </div>
                            ) : null}
                          </div>
                        ))}
                      </div>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      <div className="font-medium">
                        {service.status === "unknown"
                          ? `-- / ${formatNumber(service.total_nodes)}`
                          : `${formatNumber(service.reachable_nodes)} / ${formatNumber(service.total_nodes)}`}
                      </div>
                      {service.observed_nodes < service.total_nodes ? (
                        <div className="text-xs text-muted-foreground">
                          {t("{value0}/{value1} 节点已上报", {
                            value0: service.observed_nodes,
                            value1: service.total_nodes,
                          })}
                        </div>
                      ) : null}
                    </TableCell>
                    <TableCell className="pr-5">
                      {formatDateTime(service.last_reported_at)}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        </Panel>
      ) : (
        <EmptyState title={t("暂无回源服务")} />
      )}
    </section>
  );
}

function TunnelPeers({ tunnel }: { tunnel: WireGuardTunnel }) {
  return (
    <section className="space-y-3" aria-labelledby="wireguard-peers">
      <h2 id="wireguard-peers" className="text-base font-semibold">
        {t("边缘 Peer")}
      </h2>
      <Panel>
        <Table className="min-w-[950px]">
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">{t("节点")}</TableHead>
              <TableHead>{t("隧道 IP")}</TableHead>
              <TableHead>{t("修订")}</TableHead>
              <TableHead>{t("公钥")}</TableHead>
              <TableHead>{t("边缘出口上限")}</TableHead>
              <TableHead>{t("接收 / 发送")}</TableHead>
              <TableHead className="pr-5">{t("最近握手")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {tunnel.peers.map((peer) => (
              <TableRow key={peer.node_id}>
                <TableCell className="pl-5">
                  <div className="font-medium">{peer.node_name}</div>
                  <div className="font-mono text-xs text-muted-foreground">
                    {peer.node_public_ipv4}
                  </div>
                </TableCell>
                <TableCell className="font-mono">{peer.address}</TableCell>
                <TableCell>
                  <StatusBadge
                    status={
                      peerApplied(peer, tunnel)
                        ? "ready"
                        : peer.last_error
                          ? "failed"
                          : "pending"
                    }
                    label={`r${peer.applied_revision}`}
                  />
                  {peer.last_error ? (
                    <p className="mt-1 max-w-64 text-xs text-destructive">
                      {peer.last_error}
                    </p>
                  ) : null}
                </TableCell>
                <TableCell className="font-mono text-xs">
                  {shortHash(peer.public_key)}
                </TableCell>
                <TableCell className="text-xs">
                  {formatEgressLimit(peer.edge_egress_limit_mbps)}
                </TableCell>
                <TableCell className="text-xs">
                  {formatBytes(peer.rx_bytes)} / {formatBytes(peer.tx_bytes)}
                </TableCell>
                <TableCell className="pr-5">
                  {formatDateTime(peer.latest_handshake_at)}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Panel>
    </section>
  );
}

function Fact({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="mt-1 truncate font-mono text-xs font-medium">{value}</dd>
    </div>
  );
}

function originServiceState(status: WireGuardOriginServiceStatus) {
  switch (status) {
    case "healthy":
      return { status: "active", label: t("全部可达") };
    case "partial":
      return { status: "pending", label: t("数据不完整") };
    case "degraded":
      return { status: "pending", label: t("部分异常") };
    case "unreachable":
      return { status: "failed", label: t("全部不可达") };
    default:
      return { status: "pending", label: t("状态未知") };
  }
}

function formatOriginScheme(scheme: WireGuardOriginService["scheme"]) {
  switch (scheme) {
    case "grpc":
      return "gRPC";
    case "grpcs":
      return "gRPC TLS";
    default:
      return scheme.toUpperCase();
  }
}

function formatOriginHTTPVersion(service: WireGuardOriginService) {
  if (service.scheme === "grpc" || service.scheme === "grpcs") return "HTTP/2";
  if (service.http_version === "h2c") return "H2C";
  if (service.http_version === "http2") return "HTTP/2";
  return "HTTP/1.1";
}

function peerApplied(peer: WireGuardPeer, tunnel: WireGuardTunnel) {
  return Boolean(
    peer.public_key &&
    !peer.last_error &&
    peer.applied_revision === tunnel.revision,
  );
}

function formatEgressLimit(value: number) {
  return value > 0 ? `${formatNumber(value)} Mbps` : t("不限速");
}
