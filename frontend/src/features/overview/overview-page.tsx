import { useQuery } from "@tanstack/react-query";
import {
  Activity,
  ArrowDown,
  ArrowDownToLine,
  ArrowRight,
  ArrowUp,
  ArrowUpFromLine,
  ArrowUpDown,
  Eye,
  ExternalLink,
  RefreshCw,
  TriangleAlert,
} from "lucide-react";
import { useMemo, useState } from "react";
import { Link } from "react-router";
import { Area, AreaChart, CartesianGrid, XAxis, YAxis } from "recharts";
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
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";

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
import {
  httpStatusTone,
  toneFill,
  toneSurface,
  toneText,
  type Tone,
} from "@/lib/tones";
import type { Overview, OverviewPoint, OverviewSite } from "@/lib/types";
import { cn } from "@/lib/utils";
import { useListPagination } from "@/hooks/use-list-pagination";
import {
  usePersistentEnum,
  usePersistentState,
} from "@/hooks/use-persistent-state";
import { getLocale, t, useI18n } from "@/lib/i18n";
type Metric =
  "requests" | "downstream_bytes" | "upstream_bytes" | "error_requests";
type SiteSortKey = "name" | "requests" | "downstream_bytes" | "upstream_bytes";
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
  downstream_bytes: {
    label: "下行流量",
    color: "var(--chart-2)",
    format: formatBytes,
  },
  upstream_bytes: {
    label: "上行流量",
    color: "var(--chart-3)",
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
  const [previewSite, setPreviewSite] = useState<OverviewSite | null>(null);
  const [metric, setMetric] = usePersistentEnum<Metric>(
    "simple-cdn.overview.metric",
    [
      "requests",
      "downstream_bytes",
      "upstream_bytes",
      "error_requests",
    ] as const,
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
        ["name", "requests", "downstream_bytes", "upstream_bytes"].includes(
          candidate.key ?? "",
        ) && ["asc", "desc"].includes(candidate.direction ?? "")
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
  const metricItems = [
    {
      id: "requests",
      icon: Activity,
      label: t("请求数"),
      value: formatNumber(totals?.requests),
      meta: t("最近 24 小时"),
      tone: "info" as const,
    },
    {
      id: "downstream-bytes",
      icon: ArrowDownToLine,
      label: t("下行流量"),
      value: formatBytes(totals?.downstream_bytes),
      meta: t("客户端响应正文"),
      tone: "success" as const,
    },
    {
      id: "upstream-bytes",
      icon: ArrowUpFromLine,
      label: t("上行流量"),
      value: formatBytes(totals?.upstream_bytes),
      meta: t("客户端请求总字节"),
      tone: "info" as const,
    },
    {
      id: "errors",
      icon: TriangleAlert,
      label: t("错误请求"),
      value: formatNumber(totals?.error_requests),
      meta: t("HTTP 4xx 与 5xx"),
      tone: "warning" as const,
    },
    {
      id: "error-rate",
      icon: TriangleAlert,
      label: t("错误率"),
      value: formatPercent(errorRate, 2),
      meta: t("{value0} 个站点", {
        value0: formatNumber(query.data?.sites.length),
      }),
      tone: errorRate > 0.05 ? ("danger" as const) : ("success" as const),
    },
  ];
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
            <section aria-label={t("关键指标")}>
              <Card data-slot="metric-band" className="py-0">
                <CardContent className="grid gap-0 px-0 sm:grid-cols-2 xl:grid-cols-5">
                  {metricItems.map((item, index) => (
                    <MetricBandItem
                      key={item.id}
                      {...item}
                      className={metricBandDividers[index]}
                    />
                  ))}
                </CardContent>
              </Card>
            </section>

            <section className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_20rem]">
              <Card className="border-t-2 border-t-info">
                <CardHeader className="flex-col gap-3 border-b border-border/70 pb-4 sm:flex-row sm:items-start sm:justify-between sm:gap-4">
                  <div>
                    <CardTitle>{t("流量趋势")}</CardTitle>
                    <CardDescription>
                      {t("按小时聚合，时间为本地时区")}
                    </CardDescription>
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

              <Card className="border-t-2 border-t-success">
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

            <Card className="border-t-2 border-t-primary">
              <CardHeader className="border-b border-border/70 pb-4">
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
                            label={t("下行流量")}
                            sortKey="downstream_bytes"
                            sort={siteSort}
                            onSort={handleSiteSort}
                          />
                          <SortableSiteTableHead
                            label={t("上行流量")}
                            sortKey="upstream_bytes"
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
                              {formatBytes(site.downstream_bytes)}
                            </TableCell>
                            <TableCell className="tabular-nums">
                              {formatBytes(site.upstream_bytes)}
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
                              <div className="flex items-center justify-end gap-1">
                                <Button
                                  type="button"
                                  variant="ghost"
                                  size="icon-sm"
                                  title={t("快速预览")}
                                  onClick={() => setPreviewSite(site)}
                                >
                                  <Eye />
                                </Button>
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
                              </div>
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
            <SiteQuickPreviewSheet
              site={previewSite}
              onClose={() => setPreviewSite(null)}
            />
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
export function OverviewAreaChart({
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

  const gradientId = `overview-grad-${metric}`;

  return (
    <ChartContainer
      config={config}
      className="h-[280px] w-full aspect-auto"
      initialDimension={{
        width: 720,
        height: 280,
      }}
    >
      <AreaChart
        data={data}
        margin={{
          left: 4,
          right: 12,
          top: 12,
          bottom: 0,
        }}
        accessibilityLayer
      >
        <defs>
          <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
            <stop
              offset="5%"
              stopColor={`var(--color-${metric})`}
              stopOpacity={0.35}
            />
            <stop
              offset="95%"
              stopColor={`var(--color-${metric})`}
              stopOpacity={0.01}
            />
          </linearGradient>
        </defs>
        <CartesianGrid vertical={false} strokeDasharray="3 3" opacity={0.3} />
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
          cursor={{
            stroke: `var(--color-${metric})`,
            strokeWidth: 1,
            strokeDasharray: "4 4",
          }}
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
        <Area
          dataKey={metric}
          type="monotone"
          stroke={`var(--color-${metric})`}
          strokeWidth={2.5}
          fill={`url(#${gradientId})`}
          activeDot={{
            r: 5,
            strokeWidth: 2,
            stroke: "var(--background)",
            className: "drop-shadow-[0_0_6px_var(--color-primary)]",
          }}
          isAnimationActive={false}
        />
      </AreaChart>
    </ChartContainer>
  );
}
export const OverviewLineChart = OverviewAreaChart;

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
const metricBandDividers = [
  "",
  "border-t sm:border-t-0 sm:border-l",
  "border-t xl:border-t-0 xl:border-l",
  "border-t sm:border-l xl:border-t-0 xl:border-l",
  "border-t xl:border-t-0 xl:border-l",
] as const;

function MetricBandItem({
  icon: Icon,
  label,
  value,
  meta,
  tone,
  className,
}: {
  icon: typeof Activity;
  label: string;
  value: string;
  meta: string;
  tone: Tone;
  className?: string;
}) {
  return (
    <div
      data-slot="metric-band-item"
      className={cn(
        "group/metric flex min-h-36 flex-col px-5 py-4 transition-colors hover:bg-muted/30",
        className,
      )}
    >
      <div className="flex min-w-0 items-center gap-2">
        <span
          className={cn(
            "grid size-7 shrink-0 place-items-center rounded-md shadow-2xs transition-transform duration-200 group-hover/metric:scale-105",
            toneSurface[tone],
          )}
        >
          <Icon className={cn("size-4", toneText[tone])} aria-hidden="true" />
        </span>
        <p className="truncate text-sm font-medium text-muted-foreground">
          {label}
        </p>
      </div>
      <p className="mt-5 text-[1.65rem] font-semibold leading-none tracking-normal tabular-nums">
        {value}
      </p>
      <p className="mt-2 text-xs text-muted-foreground">{meta}</p>
      <span
        className={cn(
          "mt-auto block h-0.5 w-10 rounded-full opacity-70 transition-all duration-200 group-hover/metric:w-16",
          toneFill[tone],
        )}
        aria-hidden="true"
      />
    </div>
  );
}

function SiteQuickPreviewSheet({
  site,
  onClose,
}: {
  site: OverviewSite | null;
  onClose: () => void;
}) {
  if (!site) return null;
  const errRate = site.requests ? site.error_requests / site.requests : 0;

  return (
    <Sheet open={!!site} onOpenChange={(open) => !open && onClose()}>
      <SheetContent side="right" className="p-6 sm:max-w-md">
        <SheetHeader className="p-0 border-b pb-4">
          <SheetTitle className="flex items-center gap-2">
            <span className="font-semibold text-lg">{site.name}</span>
            <Link
              to={`/overview/sites/${encodeURIComponent(site.id)}`}
              onClick={onClose}
              className="text-muted-foreground hover:text-foreground transition-colors"
              title={t("查看完整分析")}
            >
              <ExternalLink className="size-4" />
            </Link>
          </SheetTitle>
          <SheetDescription className="truncate font-mono text-xs mt-1">
            ID: {site.id}
          </SheetDescription>
        </SheetHeader>

        <div className="mt-6 space-y-6">
          <div className="space-y-2">
            <h4 className="text-xs font-semibold text-muted-foreground uppercase tracking-wider">
              {t("绑定域名")}
            </h4>
            <div className="flex flex-wrap gap-1.5">
              {site.domains.length ? (
                site.domains.map((domain) => (
                  <span
                    key={domain}
                    className="inline-flex items-center rounded-md border bg-muted/50 px-2 py-1 font-mono text-xs text-foreground"
                  >
                    {domain}
                  </span>
                ))
              ) : (
                <span className="text-xs text-muted-foreground">
                  {t("未配置域名")}
                </span>
              )}
            </div>
          </div>

          <div className="grid grid-cols-2 gap-3">
            <div className="rounded-lg border bg-card p-3.5 shadow-2xs">
              <span className="text-xs text-muted-foreground">
                {t("请求数")}
              </span>
              <p className="mt-1 text-lg font-semibold tabular-nums">
                {formatNumber(site.requests)}
              </p>
            </div>
            <div className="rounded-lg border bg-card p-3.5 shadow-2xs">
              <span className="text-xs text-muted-foreground">
                {t("错误率")}
              </span>
              <p
                className={cn(
                  "mt-1 text-lg font-semibold tabular-nums",
                  errRate > 0.05 ? "text-destructive" : "text-foreground",
                )}
              >
                {formatPercent(errRate, 2)}
              </p>
            </div>
            <div className="rounded-lg border bg-card p-3.5 shadow-2xs">
              <span className="text-xs text-muted-foreground">
                {t("下行流量")}
              </span>
              <p className="mt-1 text-lg font-semibold tabular-nums">
                {formatBytes(site.downstream_bytes)}
              </p>
            </div>
            <div className="rounded-lg border bg-card p-3.5 shadow-2xs">
              <span className="text-xs text-muted-foreground">
                {t("上行流量")}
              </span>
              <p className="mt-1 text-lg font-semibold tabular-nums">
                {formatBytes(site.upstream_bytes)}
              </p>
            </div>
          </div>

          <div className="pt-4 border-t">
            <Button asChild className="w-full">
              <Link
                to={`/overview/sites/${encodeURIComponent(site.id)}`}
                onClick={onClose}
              >
                <span>{t("前往站点完整分析")}</span>
                <ArrowRight className="ml-2 size-4" />
              </Link>
            </Button>
          </div>
        </div>
      </SheetContent>
    </Sheet>
  );
}
