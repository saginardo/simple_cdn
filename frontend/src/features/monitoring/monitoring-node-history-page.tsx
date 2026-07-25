import { useQuery } from "@tanstack/react-query";
import { ArrowLeft, RefreshCw } from "lucide-react";
import { useMemo, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { CartesianGrid, Line, LineChart, XAxis, YAxis } from "recharts";

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
import { Checkbox } from "@/components/ui/checkbox";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import { formatDateTime, formatNumber } from "@/lib/format";
import type {
  MonitoringHistory,
  MonitoringHistoryRange,
  MonitoringHistorySeries,
} from "@/lib/types";

const historyRanges: Array<{
  value: MonitoringHistoryRange;
  label: string;
}> = [
  { value: "1h", label: "1 小时" },
  { value: "6h", label: "6 小时" },
  { value: "12h", label: "12 小时" },
  { value: "24h", label: "24 小时" },
  { value: "7d", label: "7 天" },
];

const chartColors = [
  "var(--chart-1)",
  "var(--chart-2)",
  "var(--chart-3)",
  "var(--chart-4)",
  "var(--chart-5)",
];

interface ChartSeries {
  series: MonitoringHistorySeries;
  dataKey: string;
  color: string;
}

interface HistoryChartPoint {
  timestamp: number;
  isoTime: string;
  [key: string]: number | string | null;
}

export function MonitoringNodeHistoryPage() {
  const { nodeId = "" } = useParams();
  const [range, setRange] = useState<MonitoringHistoryRange>("24h");
  const [hiddenTargets, setHiddenTargets] = useState<Set<string>>(new Set());
  const query = useQuery({
    queryKey: ["monitoring-history", nodeId, range],
    queryFn: () =>
      api<MonitoringHistory>(
        `/api/monitoring/nodes/${encodeURIComponent(nodeId)}/history?range=${range}`,
      ),
    placeholderData: (previousData) => previousData,
    refetchInterval: 30_000,
  });
  const data = query.data;
  const allChartSeries = useMemo(
    () =>
      (data?.series ?? []).map((series, index) => ({
        series,
        dataKey: `target_${index}`,
        color: chartColors[index % chartColors.length],
      })),
    [data?.series],
  );
  const visibleChartSeries = useMemo(
    () =>
      allChartSeries.filter(
        ({ series }) => !hiddenTargets.has(series.target_id),
      ),
    [allChartSeries, hiddenTargets],
  );
  const chartData = useMemo(
    () => buildChartData(visibleChartSeries),
    [visibleChartSeries],
  );
  const chartConfig = useMemo(
    () =>
      Object.fromEntries(
        visibleChartSeries.map(({ series, dataKey, color }) => [
          dataKey,
          { label: series.name, color },
        ]),
      ) satisfies ChartConfig,
    [visibleChartSeries],
  );
  const totals = useMemo(
    () => historyTotals(data?.series ?? []),
    [data?.series],
  );

  return (
    <>
      <PageHeader
        title={data?.node.name ?? "拨测历史"}
        description={
          data ? `${data.node.public_ipv4} · TCP 拨测历史` : "节点 TCP 拨测历史"
        }
        actions={
          <>
            <Button asChild variant="outline">
              <Link to="/monitoring">
                <ArrowLeft />
                返回监测
              </Link>
            </Button>
            <Button
              variant="outline"
              size="icon"
              aria-label="刷新拨测历史"
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
        {query.error ? (
          <PageError title="拨测历史加载失败" error={query.error} />
        ) : null}
        {data ? (
          <>
            <Panel>
              <dl className="grid sm:grid-cols-2 xl:grid-cols-4">
                <HistoryDatum label="节点状态">
                  <StatusBadge
                    status={data.node.status}
                    label={
                      data.node.monitor_auto_paused ? "监测暂停" : undefined
                    }
                  />
                </HistoryDatum>
                <HistoryDatum
                  label="历史目标"
                  value={`${formatNumber(data.series.length)} 个`}
                />
                <HistoryDatum
                  label="TCP 成功率"
                  value={
                    totals.attempts
                      ? `${((100 * totals.successes) / totals.attempts).toFixed(1)}%`
                      : "--"
                  }
                />
                <HistoryDatum
                  label="聚合间隔"
                  value={formatBucket(data.bucket_seconds)}
                />
              </dl>
            </Panel>

            <div className="flex justify-end">
              <Tabs
                value={range}
                onValueChange={(value) =>
                  setRange(value as MonitoringHistoryRange)
                }
              >
                <TabsList aria-label="历史时间范围">
                  {historyRanges.map((item) => (
                    <TabsTrigger key={item.value} value={item.value}>
                      {item.label}
                    </TabsTrigger>
                  ))}
                </TabsList>
              </Tabs>
            </div>

            {!data.available ? (
              <EmptyState
                title="历史拨测数据不可用"
                description={data.unavailable_reason}
              />
            ) : data.series.length ? (
              <Card>
                <CardHeader>
                  <div>
                    <CardTitle>TCP 时延趋势</CardTitle>
                    <CardDescription>
                      {formatDateTime(data.from)} 至 {formatDateTime(data.to)}
                    </CardDescription>
                  </div>
                </CardHeader>
                <CardContent className="space-y-5">
                  <div
                    className="grid gap-3 border-y py-4 sm:grid-cols-2 xl:grid-cols-3"
                    aria-label="拨测目标图例"
                  >
                    {allChartSeries.map(({ series, color }) => {
                      const visible = !hiddenTargets.has(series.target_id);
                      const targetTotals = historyTotals([series]);
                      return (
                        <div
                          key={series.target_id}
                          className="flex min-w-0 items-start gap-2"
                        >
                          <Checkbox
                            id={`history-target-${series.target_id}`}
                            checked={visible}
                            aria-label={`${visible ? "隐藏" : "显示"} ${series.name}`}
                            onCheckedChange={(checked) =>
                              setHiddenTargets((current) => {
                                const next = new Set(current);
                                if (checked) next.delete(series.target_id);
                                else next.add(series.target_id);
                                return next;
                              })
                            }
                          />
                          <label
                            htmlFor={`history-target-${series.target_id}`}
                            className="min-w-0 cursor-pointer"
                          >
                            <span className="flex items-center gap-2 text-sm font-medium">
                              <span
                                className="size-2 shrink-0 rounded-[2px]"
                                style={{ backgroundColor: color }}
                                aria-hidden="true"
                              />
                              <span className="truncate">{series.name}</span>
                              <span className="shrink-0 text-xs font-normal text-muted-foreground tabular-nums">
                                {targetTotals.attempts
                                  ? `${((100 * targetTotals.successes) / targetTotals.attempts).toFixed(1)}%`
                                  : "--"}
                              </span>
                            </span>
                            <span className="block truncate font-mono text-xs text-muted-foreground">
                              {series.address}
                            </span>
                          </label>
                        </div>
                      );
                    })}
                  </div>

                  {visibleChartSeries.length ? (
                    <MonitoringHistoryChart
                      data={chartData}
                      series={visibleChartSeries}
                      config={chartConfig}
                      range={range}
                    />
                  ) : (
                    <EmptyState title="请选择至少一个拨测目标" />
                  )}
                </CardContent>
              </Card>
            ) : (
              <EmptyState
                title="暂无历史拨测数据"
                description="节点完成新一轮拨测后，历史曲线会显示在这里"
              />
            )}
          </>
        ) : null}
      </PageBody>
    </>
  );
}

function MonitoringHistoryChart({
  data,
  series,
  config,
  range,
}: {
  data: HistoryChartPoint[];
  series: ChartSeries[];
  config: ChartConfig;
  range: MonitoringHistoryRange;
}) {
  return (
    <ChartContainer
      config={config}
      className="h-[340px] w-full aspect-auto"
      initialDimension={{ width: 720, height: 340 }}
      data-testid="monitoring-history-chart"
      data-series-count={series.length}
    >
      <LineChart
        data={data}
        margin={{ left: 4, right: 12, top: 8, bottom: 0 }}
        accessibilityLayer
      >
        <CartesianGrid vertical={false} strokeDasharray="3 3" />
        <XAxis
          dataKey="timestamp"
          type="number"
          domain={["dataMin", "dataMax"]}
          tickLine={false}
          axisLine={false}
          tickMargin={10}
          minTickGap={32}
          tickFormatter={(value) => formatAxisTime(Number(value), range)}
        />
        <YAxis
          width={58}
          tickLine={false}
          axisLine={false}
          tickFormatter={(value) => `${formatNumber(Number(value))} ms`}
        />
        <ChartTooltip
          cursor={false}
          content={
            <ChartTooltipContent
              indicator="line"
              labelFormatter={(_label, payload) =>
                formatDateTime(
                  String(
                    (payload?.[0]?.payload as HistoryChartPoint | undefined)
                      ?.isoTime ?? "",
                  ),
                )
              }
              formatter={(value, name, item) => (
                <>
                  <span
                    className="my-0.5 w-1 shrink-0 rounded-[2px]"
                    style={{ backgroundColor: item.color }}
                    aria-hidden="true"
                  />
                  <span className="flex min-w-36 flex-1 items-center justify-between gap-4">
                    <span className="text-muted-foreground">
                      {config[String(name)]?.label ?? String(name)}
                    </span>
                    <span className="font-mono font-medium tabular-nums">
                      {Number(value).toFixed(1)} ms
                    </span>
                  </span>
                </>
              )}
            />
          }
        />
        {series.map(({ series: target, dataKey, color }) => (
          <Line
            key={target.target_id}
            dataKey={dataKey}
            name={dataKey}
            type="monotone"
            stroke={color}
            strokeWidth={2}
            dot={false}
            activeDot={{ r: 4 }}
            connectNulls={false}
            isAnimationActive={false}
          />
        ))}
      </LineChart>
    </ChartContainer>
  );
}

function HistoryDatum({
  label,
  value,
  children,
}: {
  label: string;
  value?: string;
  children?: ReactNode;
}) {
  return (
    <div className="min-w-0 border-b px-4 py-3 last:border-b-0 sm:odd:border-r sm:[&:nth-last-child(-n+2)]:border-b-0 xl:border-r xl:border-b-0 xl:last:border-r-0">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="mt-1 text-sm font-medium tabular-nums">
        {children ?? value}
      </dd>
    </div>
  );
}

function buildChartData(series: ChartSeries[]): HistoryChartPoint[] {
  const points = new Map<number, HistoryChartPoint>();
  for (const item of series) {
    for (const point of item.series.points) {
      const timestamp = new Date(point.time).getTime();
      if (!Number.isFinite(timestamp)) continue;
      const row = points.get(timestamp) ?? {
        timestamp,
        isoTime: point.time,
      };
      row[item.dataKey] = point.average_latency_ms;
      points.set(timestamp, row);
    }
  }
  return [...points.values()].sort(
    (left, right) => Number(left.timestamp) - Number(right.timestamp),
  );
}

function historyTotals(series: MonitoringHistorySeries[]) {
  let attempts = 0;
  let successes = 0;
  for (const target of series) {
    for (const point of target.points) {
      attempts += point.attempts;
      successes += point.successful_attempts;
    }
  }
  return { attempts, successes };
}

function formatAxisTime(timestamp: number, range: MonitoringHistoryRange) {
  const date = new Date(timestamp);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleString("zh-CN", {
    month: range === "7d" ? "2-digit" : undefined,
    day: range === "7d" ? "2-digit" : undefined,
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

function formatBucket(seconds: number) {
  if (seconds >= 3600) return `${seconds / 3600} 小时`;
  if (seconds >= 60) return `${seconds / 60} 分钟`;
  return `${seconds} 秒`;
}
