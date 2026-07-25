import { useQuery } from "@tanstack/react-query";
import { ArrowRight, CirclePlus, RefreshCw } from "lucide-react";
import { Link } from "react-router-dom";

import {
  EmptyState,
  PageBody,
  PageError,
  PageHeader,
  PageLoading,
  Panel,
} from "@/components/page";
import { ListPagination } from "@/components/list-pagination";
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
import { formatDateTime, formatNumber } from "@/lib/format";
import type { DeploymentTask, PublishStatus, Site } from "@/lib/types";
import { useListPagination } from "@/hooks/use-list-pagination";

export function SitesPage() {
  const query = useQuery({
    queryKey: ["sites"],
    queryFn: () => api<Site[]>("/api/sites"),
    refetchInterval: 20_000,
  });
  const pagination = useListPagination(query.data ?? []);
  return (
    <>
      <PageHeader
        title="站点"
        description="域名、源站、边缘节点与发布状态"
        actions={
          <Button asChild>
            <Link to="/sites/new">
              <CirclePlus />
              添加站点
            </Link>
          </Button>
        }
      />
      <PageBody>
        {query.isLoading ? <PageLoading /> : null}
        {query.error ? <PageError error={query.error} /> : null}
        {query.data ? (
          query.data.length ? (
            <Panel>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="pl-5">站点</TableHead>
                    <TableHead>协议</TableHead>
                    <TableHead>节点</TableHead>
                    <TableHead>版本</TableHead>
                    <TableHead>发布状态</TableHead>
                    <TableHead>更新时间</TableHead>
                    <TableHead className="w-12 pr-5">
                      <span className="sr-only">管理</span>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {pagination.items.map((site) => (
                    <TableRow key={site.id}>
                      <TableCell className="pl-5">
                        <div className="font-medium">{site.name}</div>
                        <div className="max-w-sm truncate text-xs text-muted-foreground">
                          {site.domains.join(", ") || "无 HTTP 域名"}
                        </div>
                      </TableCell>
                      <TableCell className="text-sm">
                        {siteProtocol(site)}
                      </TableCell>
                      <TableCell className="tabular-nums">
                        {formatNumber(site.node_ids.length)}
                      </TableCell>
                      <TableCell className="text-sm font-medium tabular-nums">
                        V{formatNumber(site.config_version)}
                      </TableCell>
                      <TableCell>
                        <SiteStatus site={site} />
                      </TableCell>
                      <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                        {formatDateTime(site.updated_at)}
                      </TableCell>
                      <TableCell className="pr-5">
                        <Button asChild variant="ghost" size="icon-sm">
                          <Link
                            to={`/sites/${encodeURIComponent(site.id)}`}
                            aria-label={`管理 ${site.name}`}
                          >
                            <ArrowRight />
                          </Link>
                        </Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
              <ListPagination
                pagination={pagination}
                itemLabel="个站点"
                action={
                  <Button
                    variant="ghost"
                    size="icon-xs"
                    aria-label="刷新站点"
                    onClick={() => void query.refetch()}
                  >
                    <RefreshCw
                      className={query.isFetching ? "animate-spin" : undefined}
                    />
                  </Button>
                }
              />
            </Panel>
          ) : (
            <EmptyState
              title="暂无站点"
              description="创建站点后配置域名、源站与边缘节点"
            />
          )
        ) : null}
      </PageBody>
    </>
  );
}

function SiteStatus({ site }: { site: Site }) {
  const encodedID = encodeURIComponent(site.id);
  const publish = useQuery({
    queryKey: ["site-publish", site.id],
    queryFn: () => api<PublishStatus>(`/api/sites/${encodedID}/publish-status`),
    enabled: !site.deleting,
    refetchInterval: (query) =>
      activeTask(query.state.data?.task) ? 2_000 : 20_000,
  });
  if (site.deleting) return <StatusBadge status="applying" label="删除中" />;
  if (!site.enabled) return <StatusBadge status="pending" label="已停用" />;
  const publishTask = publish.data?.task;
  return (
    <StatusBadge
      status={publishTask?.status ?? (site.published ? "succeeded" : "pending")}
      label={publishTask ? undefined : site.published ? "已发布" : "待发布"}
    />
  );
}

function activeTask(task?: DeploymentTask | null) {
  return Boolean(
    task && ["queued", "dispatching", "applying"].includes(task.status),
  );
}

function siteProtocol(site: Site) {
  if (site.tcp_only) return "TCP / TLS";
  if (site.tcp_forwards.length) return "HTTP + TCP";
  const scheme = site.primary_origin.url.split(":", 1)[0]?.toLowerCase();
  if (scheme === "grpc" || scheme === "grpcs") return "gRPC";
  if (scheme === "ws" || scheme === "wss") return "WebSocket";
  return "HTTP / WS / SSE";
}
