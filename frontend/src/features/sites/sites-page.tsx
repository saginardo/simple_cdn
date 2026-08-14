import { useQuery } from "@tanstack/react-query";
import { ArrowRight, CirclePlus, RefreshCw } from "lucide-react";
import { Link } from "react-router";
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
import type { PublishStatus, Site } from "@/lib/types";
import { useListPagination } from "@/hooks/use-list-pagination";
import { t, useI18n } from "@/lib/i18n";
import { activeTask, taskMatchesCurrentSite } from "./publish-status";
export function SitesPage() {
  useI18n();
  const query = useQuery({
    queryKey: ["sites"],
    queryFn: () => api<Site[]>("/api/sites"),
    refetchInterval: 20_000,
  });
  const pagination = useListPagination(query.data ?? []);
  return (
    <>
      <PageHeader
        title={t("站点")}
        description={t("域名、源站、边缘节点与发布状态")}
        actions={
          <Button asChild>
            <Link to="/sites/new">
              <CirclePlus />
              {t("添加站点")}
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
                    <TableHead className="pl-5">{t("站点")}</TableHead>
                    <TableHead>{t("回源类型")}</TableHead>
                    <TableHead>{t("节点")}</TableHead>
                    <TableHead>{t("版本")}</TableHead>
                    <TableHead>{t("发布状态")}</TableHead>
                    <TableHead>{t("更新时间")}</TableHead>
                    <TableHead className="w-12 pr-5">
                      <span className="sr-only">{t("管理")}</span>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {pagination.items.map((site) => (
                    <TableRow key={site.id}>
                      <TableCell className="pl-5">
                        <div className="font-medium">{site.name}</div>
                        <div className="max-w-sm truncate text-xs text-muted-foreground">
                          {site.domains.join(", ") || t("无 HTTP 域名")}
                        </div>
                      </TableCell>
                      <TableCell className="text-sm">
                        {siteOriginType(site)}
                      </TableCell>
                      <TableCell className="tabular-nums">
                        <span>{formatNumber(site.node_ids.length)}</span>
                        {(site.backup_node_ids?.length ?? 0) > 0 ? (
                          <span className="ml-1 text-xs text-muted-foreground">
                            + {formatNumber(site.backup_node_ids.length)}{" "}
                            {t("备用")}
                          </span>
                        ) : null}
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
                            aria-label={t("管理 {value0}", {
                              value0: site.name,
                            })}
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
                itemLabel={t("个站点")}
                action={
                  <Button
                    variant="ghost"
                    size="icon-xs"
                    aria-label={t("刷新站点")}
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
              title={t("暂无站点")}
              description={t("创建站点后配置域名、源站与边缘节点")}
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
  if (site.deleting)
    return <StatusBadge status="applying" label={t("删除中")} />;
  if (!site.enabled)
    return <StatusBadge status="pending" label={t("已停用")} />;
  const task = publish.data?.task;
  const publishTask = taskMatchesCurrentSite(task, site) ? task : undefined;
  return (
    <StatusBadge
      status={publishTask?.status ?? (site.published ? "succeeded" : "pending")}
      label={
        publishTask ? undefined : site.published ? t("已发布") : t("待发布")
      }
    />
  );
}
function siteOriginType(site: Site) {
  if (site.tcp_only) return t("直连");
  const origins = [site.primary_origin, site.backup_origin].filter(
    (origin) => origin?.enabled,
  );
  const direct = origins.some((origin) => !origin?.wireguard_tunnel_id);
  const tunneled = origins.some((origin) =>
    Boolean(origin?.wireguard_tunnel_id),
  );
  if (direct && tunneled) return t("直连 + 隧道");
  return tunneled ? t("隧道") : t("直连");
}
