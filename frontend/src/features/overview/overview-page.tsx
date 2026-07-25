import { useQuery } from "@tanstack/react-query";
import {
  Activity,
  ArrowDown,
  ArrowRight,
  ArrowUp,
  ArrowUpDown,
  DatabaseZap,
  RefreshCw,
  TriangleAlert,
} from "lucide-react";
import { useMemo } from "react";
import { Link } from "react-router-dom";
import { CartesianGrid, Line, LineChart, XAxis, YAxis } from "recharts";
import {
  EmptyState,
  PageBody,
  PageError,
  PageHeader,
  PageLoading,
} from "@/components/page";
import { ListPagination } from "@/components/list-pagination";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
  type ChartConfig,
} from "@/components/ui/chart";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import {
  formatBytes,
  formatCompact,
  formatDateTime,
  formatNumber,
  formatPercent,
} from "@/lib/format";
import { httpStatusTone, toneFill, toneText, type Tone } from "@/lib/tones";
import type { Overview, OverviewPoint, OverviewSite } from "@/lib/types";
import { cn } from "@/lib/utils";
import { useListPagination } from "@/hooks/use-list-pagination";
import {
  usePersistentEnum,
  usePersistentState,
} from "@/hooks/use-persistent-state";
import { getLocale, t, useI18n } from "@/lib/i18n";
type Metric = "requests" | "bytes" | "error_requests";
type SiteSortKey = "name" | "requests" | "bytes";
type SortDirection = "asc" | "desc";
interface SiteSort {
  key: SiteSortKey;
  direction: SortDirection;
}
const metricConfig: Record<
  Metric,
  {
    label: string;
    color: string;
    format: (value: number) => string;
  }
> = {
  requests: {
    label: "请求数",
    color: "var(--chart-1)",
    format: formatCompact,
  },
  bytes: {
    label: "传输量",
    color: "var(--chart-2)",
    format: formatBytes,
  },
  error_requests: {
    label: "错误请求",
    color: "var(--chart-5)",
    format: formatCompact,
  },
};
export function OverviewPage() {
  useI18n();
  const [metric, setMetric] = usePersistentEnum<Metric>(
    "simple-cdn.overview.metric",
    ["requests", "bytes", "error_requests"] as const,
    "requests",
  );
  const [siteSort, setSiteSort] = usePersistentState<SiteSort>(
    "simple-cdn.overview.site-sort",
    {
      key: "requests",
      direction: "desc",
    },
    (value): value is SiteSort => {
      if (!value || typeof value !== "object") return false;
      const candidate = value as Partial<SiteSort>;
      return (
        ["name", "requests", "bytes"].includes(candidate.key ?? "") &&
        ["asc", "desc"].includes(candidate.direction ?? "")
      );
    },
  );
  const query = useQuery({
    queryKey: ["overview"],
    queryFn: () => api<Overview>("/api/overview"),
    refetchInterval: 30_000,
  });
  const chartData = useMemo(
    () => (query.data?.series ?? []).map(chartPoint),
    [query.data],
  );
  const totals = query.data?.totals;
  const errorRate = totals?.requests
    ? totals.error_requests / totals.requests
    : 0;
  const sortedSites = useMemo(
    () => sortOverviewSites(query.data?.sites ?? [], siteSort),
    [query.data?.sites, siteSort],
  );
  const sitesPagination = useListPagination(sortedSites);
  const handleSiteSort = (key: SiteSortKey) => {
    setSiteSort((current) => ({
      key,
      direction:
        current.key === key
          ? current.direction === "asc"
            ? "desc"
            : "asc"
          : defaultSiteSortDirection(key),
    }));
    sitesPagination.setPage(1);
  };
  return (
    <>
      <PageHeader
        title={t("概览")}
        description={t("最近 24 小时控制面流量与服务状态")}
        actions={
          <>
            <span className="hidden text-xs text-muted-foreground sm:inline">
              {query.data
                ? t("更新于 {value0}", {
                    value0: formatDateTime(query.data.to),
                  })
                : t("等待数据")}
            </span>
            <Button
              variant="outline"
              size="icon-sm"
              aria-label={t("刷新概览")}
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              <RefreshCw
                className={query.isFetching ? "animate-spin" : undefined}
              />
            </Button>
          </>
        }
      />
      <PageBody>
        {query.isLoading ? <PageLoading /> : null}
        {query.error ? <PageError error={query.error} /> : null}
        {query.data ? (
          <>
            <section
              className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4"
              aria-label={t("关键指标")}
            >
              <MetricCard
                icon={Activity}
                label={t("请求数")}
                value={formatNumber(totals?.requests)}
                meta={t("最近 24 小时")}
                tone="info"
              />
              <MetricCard
                icon={DatabaseZap}
                label={t("传输量")}
                value={formatBytes(totals?.bytes)}
                meta={t("边缘下行流量")}
                tone="success"
              />
              <MetricCard
                icon={TriangleAlert}
                label={t("错误请求")}
                value={formatNumber(totals?.error_requests)}
                meta={t("HTTP 4xx 与 5xx")}
                tone="warning"
              />
              <MetricCard
                icon={TriangleAlert}
                label={t("错误率")}
                value={formatPercent(errorRate, 2)}
                meta={t("{value0} 个站点", {
                  value0: formatNumber(query.data.sites.length),
                })}
                tone={errorRate > 0.05 ? "danger" : "success"}
              />
            </section>

            <section className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_20rem]">
              <Card>
                <CardHeader className="flex-row items-start justify-between gap-4">
                  <div>
                    <CardTitle>{t("流量趋势")}</CardTitle>
                    <CardDescription>
                      {t("按小时聚合，时间为本地时区")}
                    </CardDescription>
                  </div>
                  <Tabs
                    value={metric}
                    onValueChange={(value) => setMetric(value as Metric)}
                  >
                    <TabsList>
                      <TabsTrigger value="requests">{t("请求")}</TabsTrigger>
                      <TabsTrigger value="bytes">{t("流量")}</TabsTrigger>
                      <TabsTrigger value="error_requests">
                        {t("错误")}
                      </TabsTrigger>
                    </TabsList>
                  </Tabs>
                </CardHeader>
                <CardContent>
                  <OverviewLineChart data={chartData} metric={metric} />
                </CardContent>
              </Card>

              <Card>
                <CardHeader>
                  <CardTitle>{t("状态码分布")}</CardTitle>
                  <CardDescription>
                    {formatNumber(totals?.requests)}
                    {t(" 次请求")}
                  </CardDescription>
                </CardHeader>
                <CardContent>
                  {query.data.status_codes.length ? (
                    <div className="space-y-3">
                      {query.data.status_codes.slice(0, 8).map((item) => {
                        const ratio = totals?.requests
                          ? item.requests / totals.requests
                          : 0;
                        return (
                          <div key={item.code}>
                            <div className="mb-1 flex items-center justify-between text-xs">
                              <span className="font-mono font-medium">
                                HTTP {item.code}
                              </span>
                              <span className="text-muted-foreground">
                                {formatPercent(ratio)} ·{" "}
                                {formatCompact(item.requests)}
                              </span>
                            </div>
                            <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                              <div
                                className={cn(
                                  "h-full",
                                  toneFill[httpStatusTone(item.code)],
                                )}
                                style={{
                                  width: `${Math.max(1, ratio * 100)}%`,
                                }}
                              />
                            </div>
                          </div>
                        );
                      })}
                    </div>
                  ) : (
                    <EmptyState title={t("暂无状态码数据")} />
                  )}
                </CardContent>
              </Card>
            </section>

            <Card>
              <CardHeader>
                <CardTitle>{t("站点流量")}</CardTitle>
                <CardDescription>
                  {t("最近 24 小时聚合，可进入站点分析详情")}
                </CardDescription>
              </CardHeader>
              <CardContent className="px-0">
                {query.data.sites.length ? (
                  <>
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <SortableSiteTableHead
                            className="pl-6"
                            label={t("站点")}
                            sortKey="name"
                            sort={siteSort}
                            onSort={handleSiteSort}
                          />
                          <SortableSiteTableHead
                            label={t("请求数")}
                            sortKey="requests"
                            sort={siteSort}
                            onSort={handleSiteSort}
                          />
                          <SortableSiteTableHead
                            label={t("传输量")}
                            sortKey="bytes"
                            sort={siteSort}
                            onSort={handleSiteSort}
                          />
                          <TableHead>{t("错误率")}</TableHead>
                          <TableHead className="w-12 pr-6">
                            <span className="sr-only">{t("详情")}</span>
                          </TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {sitesPagination.items.map((site) => (
                          <TableRow key={site.id}>
                            <TableCell className="pl-6">
                              <div className="font-medium">{site.name}</div>
                              <div className="max-w-md truncate text-xs text-muted-foreground">
                                {site.domains.join(", ") || t("未配置域名")}
                              </div>
                            </TableCell>
                            <TableCell className="tabular-nums">
                              {formatNumber(site.requests)}
                            </TableCell>
                            <TableCell className="tabular-nums">
                              {formatBytes(site.bytes)}
                            </TableCell>
                            <TableCell className="tabular-nums">
                              {formatPercent(
                                site.requests
                                  ? site.error_requests / site.requests
                                  : 0,
                                2,
                              )}
                            </TableCell>
                            <TableCell className="pr-6">
                              <Button asChild variant="ghost" size="icon-sm">
                                <Link
                                  to={`/overview/sites/${encodeURIComponent(site.id)}`}
                                  aria-label={t("查看 {value0} 分析", {
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
                      pagination={sitesPagination}
                      itemLabel={t("个站点")}
                    />
                  </>
                ) : (
                  <div className="px-6 pb-6">
                    <EmptyState
                      title={t("暂无站点")}
                      description={t(
                        "添加站点并产生流量后，这里会显示聚合数据",
                      )}
                    />
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
function SortableSiteTableHead({
  className,
  label,
  sortKey,
  sort,
  onSort,
}: {
  className?: string;
  label: string;
  sortKey: SiteSortKey;
  sort: SiteSort;
  onSort: (key: SiteSortKey) => void;
}) {
  const active = sort.key === sortKey;
  const nextDirection = active
    ? sort.direction === "asc"
      ? "desc"
      : "asc"
    : defaultSiteSortDirection(sortKey);
  const SortIcon = active
    ? sort.direction === "asc"
      ? ArrowUp
      : ArrowDown
    : ArrowUpDown;
  const actionLabel = t("按{value0}{value1}排序", {
    value0: label,
    value1: nextDirection === "asc" ? t("升序") : t("降序"),
  });
  return (
    <TableHead
      className={className}
      aria-sort={
        active
          ? sort.direction === "asc"
            ? "ascending"
            : "descending"
          : "none"
      }
    >
      <Button
        type="button"
        variant="ghost"
        size="sm"
        className="-ml-3 font-medium"
        aria-label={actionLabel}
        title={actionLabel}
        onClick={() => onSort(sortKey)}
      >
        <span>{label}</span>
        <SortIcon
          data-icon="inline-end"
          className={active ? "text-foreground" : "text-muted-foreground"}
          aria-hidden="true"
        />
      </Button>
    </TableHead>
  );
}
function defaultSiteSortDirection(key: SiteSortKey): SortDirection {
  return key === "name" ? "asc" : "desc";
}
function sortOverviewSites(
  sites: readonly OverviewSite[],
  sort: SiteSort,
): OverviewSite[] {
  const siteNameCollator = new Intl.Collator(getLocale(), {
    numeric: true,
    sensitivity: "base",
  });
  return [...sites].sort((left, right) => {
    const comparison =
      sort.key === "name"
        ? siteNameCollator.compare(siteName(left), siteName(right))
        : left[sort.key] - right[sort.key];
    if (comparison) {
      return sort.direction === "desc" ? -comparison : comparison;
    }
    const nameComparison = siteNameCollator.compare(
      siteName(left),
      siteName(right),
    );
    return nameComparison || siteNameCollator.compare(left.id, right.id);
  });
}
function siteName(site: OverviewSite) {
  return site.name || site.id || t("未命名站点");
}
export function OverviewLineChart({
  data,
  metric,
}: {
  data: Array<
    OverviewPoint & {
      label: string;
    }
  >;
  metric: Metric;
}) {
  const selected = {
    ...metricConfig[metric],
    label: t(metricConfig[metric].label),
  };
  const config = {
    [metric]: {
      label: selected.label,
      color: selected.color,
    },
  } satisfies ChartConfig;
  return (
    <ChartContainer
      config={config}
      className="h-[280px] w-full aspect-auto"
      initialDimension={{
        width: 720,
        height: 280,
      }}
    >
      <LineChart
        data={data}
        margin={{
          left: 4,
          right: 12,
          top: 8,
          bottom: 0,
        }}
        accessibilityLayer
      >
        <CartesianGrid vertical={false} strokeDasharray="3 3" />
        <XAxis
          dataKey="label"
          tickLine={false}
          axisLine={false}
          tickMargin={10}
          minTickGap={30}
        />
        <YAxis
          width={58}
          tickLine={false}
          axisLine={false}
          tickFormatter={(value) => selected.format(Number(value))}
        />
        <ChartTooltip
          cursor={false}
          content={
            <ChartTooltipContent
              indicator="line"
              labelKey="label"
              formatter={(value) => (
                <div className="flex min-w-32 items-center justify-between gap-4">
                  <span className="text-muted-foreground">
                    {selected.label}
                  </span>
                  <span className="font-mono font-medium tabular-nums">
                    {selected.format(Number(value))}
                  </span>
                </div>
              )}
            />
          }
        />
        <Line
          dataKey={metric}
          type="monotone"
          stroke={`var(--color-${metric})`}
          strokeWidth={2}
          dot={false}
          activeDot={{
            r: 4,
          }}
          isAnimationActive={false}
        />
      </LineChart>
    </ChartContainer>
  );
}
export function chartPoint(point: OverviewPoint): OverviewPoint & {
  label: string;
} {
  return {
    ...point,
    label: new Date(point.time).toLocaleString(getLocale(), {
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      hour12: false,
    }),
  };
}
function MetricCard({
  icon: Icon,
  label,
  value,
  meta,
  tone,
}: {
  icon: typeof Activity;
  label: string;
  value: string;
  meta: string;
  tone: Tone;
}) {
  return (
    <Card>
      <CardContent className="flex items-start justify-between gap-3 p-5">
        <div>
          <p className="text-sm text-muted-foreground">{label}</p>
          <p className="mt-2 text-2xl font-semibold tracking-normal tabular-nums">
            {value}
          </p>
          <p className="mt-1 text-xs text-muted-foreground">{meta}</p>
        </div>
        <Icon
          className={cn("mt-0.5 size-4", toneText[tone])}
          aria-hidden="true"
        />
      </CardContent>
    </Card>
  );
}
