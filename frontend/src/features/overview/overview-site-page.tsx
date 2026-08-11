import { useQuery } from "@tanstack/react-query";
import { ArrowLeft, Settings2 } from "lucide-react";
import { useMemo } from "react";
import { Link, useParams } from "react-router";
import {
  EmptyState,
  PageBody,
  PageError,
  PageHeader,
  PageLoading,
  Panel,
} from "@/components/page";
import { ListPagination } from "@/components/list-pagination";
import {
  OverviewAreaChart,
  chartPoint,
} from "@/features/overview/overview-page";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import { useListPagination } from "@/hooks/use-list-pagination";
import { usePersistentEnum } from "@/hooks/use-persistent-state";
import { formatBytes, formatNumber, formatPercent } from "@/lib/format";
import type { Overview } from "@/lib/types";
import { t, useI18n } from "@/lib/i18n";
type Metric =
  "requests" | "downstream_bytes" | "upstream_bytes" | "error_requests";
export function OverviewSitePage() {
  useI18n();
  const { siteId = "" } = useParams();
  const [metric, setMetric] = usePersistentEnum<Metric>(
    "simple-cdn.overview.site.metric",
    [
      "requests",
      "downstream_bytes",
      "upstream_bytes",
      "error_requests",
    ] as const,
    "requests",
  );
  const query = useQuery({
    queryKey: ["overview"],
    queryFn: () => api<Overview>("/api/overview"),
    refetchInterval: 30_000,
  });
  const site = query.data?.sites.find((item) => item.id === siteId);
  const chartData = useMemo(() => (site?.series ?? []).map(chartPoint), [site]);
  const statusPagination = useListPagination(site?.status_codes ?? []);
  return (
    <>
      <PageHeader
        title={site?.name ?? t("站点请求详情")}
        description={site?.domains.join(", ") || t("最近 24 小时站点流量")}
        actions={
          <>
            <Button asChild variant="outline">
              <Link to="/overview">
                <ArrowLeft />
                {t("返回概览")}
              </Link>
            </Button>
            {site ? (
              <Button asChild>
                <Link to={`/sites/${encodeURIComponent(site.id)}`}>
                  <Settings2 />
                  {t("管理站点")}
                </Link>
              </Button>
            ) : null}
          </>
        }
      />
      <PageBody>
        {query.isLoading ? <PageLoading /> : null}
        {query.error ? <PageError error={query.error} /> : null}
        {query.data && !site ? (
          <EmptyState
            title={t("未找到站点")}
            description={t("该站点可能已被删除")}
          />
        ) : null}
        {site ? (
          <>
            <section className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
              <Summary
                label={t("请求数")}
                value={formatNumber(site.requests)}
              />
              <Summary
                label={t("下行流量")}
                value={formatBytes(site.downstream_bytes)}
              />
              <Summary
                label={t("上行流量")}
                value={formatBytes(site.upstream_bytes)}
              />
              <Summary
                label={t("错误率")}
                value={formatPercent(
                  site.requests ? site.error_requests / site.requests : 0,
                  2,
                )}
              />
            </section>
            <Card>
              <CardHeader className="flex-col items-start justify-between gap-4 sm:flex-row">
                <div>
                  <CardTitle>{t("站点趋势")}</CardTitle>
                  <CardDescription>{t("按小时聚合")}</CardDescription>
                </div>
                <Tabs
                  className="w-full sm:w-auto"
                  value={metric}
                  onValueChange={(value) => setMetric(value as Metric)}
                >
                  <TabsList className="w-full sm:w-auto">
                    <TabsTrigger value="requests">{t("请求")}</TabsTrigger>
                    <TabsTrigger value="downstream_bytes">
                      {t("下行")}
                    </TabsTrigger>
                    <TabsTrigger value="upstream_bytes">
                      {t("上行")}
                    </TabsTrigger>
                    <TabsTrigger value="error_requests">
                      {t("错误")}
                    </TabsTrigger>
                  </TabsList>
                </Tabs>
              </CardHeader>
              <CardContent>
                <OverviewAreaChart data={chartData} metric={metric} />
              </CardContent>
            </Card>
            <Card>
              <CardHeader>
                <CardTitle>{t("HTTP 状态码")}</CardTitle>
                <CardDescription>{t("按请求量降序")}</CardDescription>
              </CardHeader>
              <CardContent className="px-0">
                {site.status_codes.length ? (
                  <>
                    <div className="grid gap-3 px-6 sm:grid-cols-2 xl:grid-cols-3">
                      {statusPagination.items.map((item) => (
                        <div
                          key={item.code}
                          className="flex items-center justify-between rounded-lg border px-4 py-3"
                        >
                          <span className="font-mono text-sm">
                            HTTP {item.code}
                          </span>
                          <span className="text-sm tabular-nums text-muted-foreground">
                            {formatNumber(item.requests)} ·{" "}
                            {formatPercent(
                              site.requests ? item.requests / site.requests : 0,
                            )}
                          </span>
                        </div>
                      ))}
                    </div>
                    <ListPagination
                      pagination={statusPagination}
                      itemLabel={t("个状态码")}
                      className="mt-4"
                    />
                  </>
                ) : (
                  <div className="px-6">
                    <EmptyState title={t("暂无状态码数据")} />
                  </div>
                )}
              </CardContent>
            </Card>
          </>
        ) : null}
      </PageBody>
    </>
  );
}
function Summary({ label, value }: { label: string; value: string }) {
  return (
    <Panel className="px-5 py-4">
      <p className="text-sm text-muted-foreground">{label}</p>
      <p className="mt-2 text-2xl font-semibold tabular-nums">{value}</p>
    </Panel>
  );
}
