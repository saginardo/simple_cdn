import { useQuery } from "@tanstack/react-query";
import {
  Activity,
  AlertTriangle,
  ArrowUpRight,
  CheckCheck,
  ChevronRight,
  CircleHelp,
  Clock3,
  RefreshCw,
  Search,
  ShieldCheck,
} from "lucide-react";
import { useEffect, useState } from "react";
import { Link } from "react-router";

import {
  EmptyState,
  PageBody,
  PageError,
  PageHeader,
  PageLoading,
  Panel,
} from "@/components/page";
import { StatBand, StatItem } from "@/components/stat-band";
import { StatusBadge } from "@/components/status-badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { t, useI18n } from "@/lib/i18n";
import type {
  HealthCheck,
  HealthRecovery,
  HealthState,
  SystemHealthSnapshot,
} from "@/lib/types";

const categoryNames: Record<string, string> = {
  control: "控制面",
  node: "节点",
  site: "站点可用性",
  publication: "配置发布",
  certificate: "证书",
  backup: "备份恢复",
  upgrade: "升级",
};
const stateNames: Record<HealthState, string> = {
  healthy: "正常",
  warning: "需关注",
  critical: "严重",
  unknown: "待确认",
  disabled: "未启用",
};
const badgeStates: Record<HealthState, string> = {
  healthy: "succeeded",
  warning: "partial",
  critical: "failed",
  unknown: "unreported",
  disabled: "not_requested",
};
const attention = (state: HealthState) =>
  ["critical", "warning", "unknown"].includes(state);

function HealthBadge({ state }: { state: HealthState }) {
  return <StatusBadge status={badgeStates[state]} label={stateNames[state]} />;
}

export function HealthPage() {
  useI18n();
  const query = useQuery({
    queryKey: ["system-health"],
    queryFn: () => api<SystemHealthSnapshot>("/api/system/health"),
    refetchInterval: 15_000,
  });
  const [now, setNow] = useState(Date.now());
  const [tab, setTab] = useState("checks");
  const [status, setStatus] = useState("attention");
  const [category, setCategory] = useState("all");
  const [node, setNode] = useState("all");
  const [site, setSite] = useState("all");
  const [search, setSearch] = useState("");
  const [selectedID, setSelectedID] = useState<string>();
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 15_000);
    return () => clearInterval(timer);
  }, []);
  const data = query.data;
  const validUntil = data?.valid_until ? Date.parse(data.valid_until) : NaN;
  const stale = Boolean(
    data && (data.stale || !Number.isFinite(validUntil) || validUntil <= now),
  );
  const checks = (data?.checks ?? []).map((check) =>
    stale && check.state === "healthy"
      ? {
          ...check,
          state: "unknown" as HealthState,
          summary: "检查结果已过期",
          since:
            Number.isFinite(validUntil) && validUntil <= now
              ? new Date(validUntil).toISOString()
              : check.since,
        }
      : check,
  );
  const critical = checks.filter((c) => c.state === "critical").length;
  const warning = checks.filter((c) => c.state === "warning").length;
  const unknown = checks.filter((c) => c.state === "unknown").length;
  const overall: HealthState = critical
    ? "critical"
    : warning
      ? "warning"
      : unknown || stale || query.isError
        ? "unknown"
        : (data?.state ?? "unknown");
  const nodeNames = Object.fromEntries(
    (data?.nodes ?? []).map((n) => [n.id, n.name]),
  );
  const siteNames = Object.fromEntries(
    (data?.sites ?? []).map((s) => [s.id, s.name]),
  );
  const resourceText = (item: HealthCheck | HealthRecovery) =>
    [
      ...item.node_ids.map((id) => nodeNames[id] ?? id),
      ...item.site_ids.map((id) => siteNames[id] ?? id),
    ].join(" · ");
  const matches = (item: HealthCheck | HealthRecovery) =>
    (node === "all" || item.node_ids.includes(node)) &&
    (site === "all" || item.site_ids.includes(site)) &&
    (!search.trim() ||
      `${t(item.title)} ${resourceText(item)} ${"summary" in item ? t(item.summary) : ""}`
        .toLocaleLowerCase()
        .includes(search.trim().toLocaleLowerCase()));
  const visible = checks.filter(
    (c) =>
      matches(c) &&
      (category === "all" || c.category === category) &&
      (status === "all" ||
        (status === "attention" ? attention(c.state) : c.state === status)),
  );
  const recoveries = (data?.recoveries ?? []).filter(matches);
  const selected = checks.find((c) => c.id === selectedID);
  const reset = () => {
    setSearch("");
    setStatus("attention");
    setCategory("all");
    setNode("all");
    setSite("all");
  };
  return (
    <>
      <PageHeader
        title={t("状态")}
        description={t(
          "集中检查服务状态、配置一致性与恢复保障，按影响优先处理问题。",
        )}
        actions={
          <Button
            variant="outline"
            onClick={() => void query.refetch()}
            disabled={query.isFetching}
          >
            <RefreshCw className={query.isFetching ? "animate-spin" : ""} />
            {t("刷新结果")}
          </Button>
        }
      />
      <PageBody>
        {query.isLoading ? <PageLoading /> : null}
        {query.error ? (
          <PageError
            title={t("健康数据加载失败")}
            error={query.error}
            onRetry={() => void query.refetch()}
          />
        ) : null}
        {data ? (
          <>
            <StatBand className="grid-cols-2 xl:grid-cols-4">
              <StatItem
                icon={ShieldCheck}
                label={t("整体状态")}
                value={t(stateNames[overall])}
                detail={t("按当前检查结果汇总")}
                tone={
                  overall === "critical"
                    ? "danger"
                    : overall === "healthy"
                      ? "success"
                      : "warning"
                }
              />
              <StatItem
                icon={AlertTriangle}
                label={t("需要处理")}
                value={critical + warning}
                detail={t("严重 {value0} 项 · 需关注 {value1} 项", {
                  value0: critical,
                  value1: warning,
                })}
                tone={critical ? "danger" : warning ? "warning" : "neutral"}
              />
              <StatItem
                icon={CircleHelp}
                label={t("待确认")}
                value={unknown}
                detail={t("缺失、过期或尚未完成的检查")}
                tone={unknown ? "warning" : "neutral"}
              />
              <StatItem
                icon={CheckCheck}
                label={t("最近恢复")}
                value={
                  data.recoveries.filter((r) => r.outcome === "resolved").length
                }
                detail={t("保留最近 30 天，最多 100 条")}
              />
            </StatBand>
            <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
              <span className="flex items-center gap-1.5">
                <Clock3 className="size-3.5" />
                {t("最近采集：{value0}", {
                  value0: data.collected_at
                    ? formatDateTime(data.collected_at)
                    : t("等待首次采集"),
                })}
              </span>
              <span>{t("后台每 30 秒检查，页面每 15 秒更新")}</span>
            </div>
            {stale ? (
              <Alert>
                <CircleHelp />
                <AlertTitle>{t("健康数据已过期或尚未采集")}</AlertTitle>
                <AlertDescription>
                  {t(
                    "旧结果仅供参考，请等待后台重新采集。刷新按钮只读取最新结果，不触发外部探测。",
                  )}
                </AlertDescription>
              </Alert>
            ) : null}
            <Tabs value={tab} onValueChange={setTab}>
              <TabsList aria-label={t("状态视图")}>
                <TabsTrigger value="checks">{t("检查与问题")}</TabsTrigger>
                <TabsTrigger value="recoveries">{t("恢复记录")}</TabsTrigger>
              </TabsList>
              <div className="mt-4 grid gap-3 sm:grid-cols-2 xl:grid-cols-5">
                <div className="grid gap-1.5">
                  <Label htmlFor="health-search">{t("搜索")}</Label>
                  <div className="relative">
                    <Search className="pointer-events-none absolute left-3 top-2.5 size-4 text-muted-foreground" />
                    <Input
                      id="health-search"
                      className="pl-9"
                      placeholder={t("检查名称、节点或站点")}
                      value={search}
                      onChange={(e) => setSearch(e.target.value)}
                    />
                  </div>
                </div>
                {tab === "checks" ? (
                  <>
                    <HealthFilter
                      id="health-status"
                      label={t("检查状态")}
                      value={status}
                      onChange={setStatus}
                      options={[
                        { value: "attention", label: t("待处理问题") },
                        { value: "all", label: t("全部状态") },
                        ...Object.entries(stateNames).map(([value, label]) => ({
                          value,
                          label: t(label),
                        })),
                      ]}
                    />
                    <HealthFilter
                      id="health-category"
                      label={t("检查范围")}
                      value={category}
                      onChange={setCategory}
                      options={[
                        { value: "all", label: t("全部范围") },
                        ...Object.entries(categoryNames).map(
                          ([value, label]) => ({ value, label: t(label) }),
                        ),
                      ]}
                    />
                  </>
                ) : null}
                <HealthFilter
                  id="health-node"
                  label={t("节点")}
                  value={node}
                  onChange={setNode}
                  options={[
                    { value: "all", label: t("全部节点") },
                    ...data.nodes.map((n) => ({ value: n.id, label: n.name })),
                  ]}
                />
                <HealthFilter
                  id="health-site"
                  label={t("站点")}
                  value={site}
                  onChange={setSite}
                  options={[
                    { value: "all", label: t("全部站点") },
                    ...data.sites.map((s) => ({ value: s.id, label: s.name })),
                  ]}
                />
              </div>
              <div className="mt-3 flex items-center justify-between gap-2 text-xs text-muted-foreground">
                <span aria-live="polite">
                  {tab === "checks"
                    ? t("显示 {value0} / {value1} 项检查", {
                        value0: visible.length,
                        value1: checks.length,
                      })
                    : t("显示 {value0} 条记录", { value0: recoveries.length })}
                </span>
                <Button size="sm" variant="ghost" onClick={reset}>
                  {t("重置筛选")}
                </Button>
              </div>
              <TabsContent value="checks" className="mt-3">
                {visible.length ? (
                  <Panel>
                    <ul aria-label={t("健康检查列表")} className="divide-y">
                      {visible.map((check) => (
                        <li key={check.id}>
                          <button
                            type="button"
                            className="grid w-full gap-3 px-4 py-4 text-left transition-colors hover:bg-muted/40 focus-visible:outline-2 focus-visible:outline-ring sm:grid-cols-[100px_minmax(0,1fr)_auto] sm:items-start sm:px-5"
                            onClick={() => setSelectedID(check.id)}
                            aria-label={t("查看检查：{value0}", {
                              value0: `${t(check.title)} ${resourceText(check)}`,
                            })}
                          >
                            <div>
                              <HealthBadge state={check.state} />
                              <div className="mt-1.5 text-xs text-muted-foreground">
                                {t(
                                  categoryNames[check.category] ??
                                    check.category,
                                )}
                              </div>
                            </div>
                            <div className="min-w-0">
                              <div className="break-words text-sm font-medium">
                                {t(check.title)}
                              </div>
                              <p className="mt-1 break-words text-sm text-muted-foreground">
                                {t(check.summary)}
                              </p>
                              {resourceText(check) ? (
                                <p className="mt-2 break-words text-xs text-muted-foreground">
                                  {resourceText(check)}
                                </p>
                              ) : null}
                            </div>
                            <div className="flex items-center gap-2 text-xs text-muted-foreground sm:justify-end">
                              <span>{t("查看证据")}</span>
                              <ChevronRight className="size-4" />
                            </div>
                          </button>
                        </li>
                      ))}
                    </ul>
                  </Panel>
                ) : (
                  <EmptyState
                    icon={Activity}
                    title={t("当前筛选下没有检查项")}
                    description={t(
                      "可以查看全部检查，或调整节点、站点与搜索条件。",
                    )}
                    action={
                      <Button
                        variant="outline"
                        onClick={() => {
                          reset();
                          setStatus("all");
                        }}
                      >
                        {t("查看全部检查")}
                      </Button>
                    }
                  />
                )}
              </TabsContent>
              <TabsContent value="recoveries" className="mt-3">
                {recoveries.length ? (
                  <Panel>
                    <ul className="divide-y" aria-label={t("健康恢复记录")}>
                      {recoveries.map((event, index) => (
                        <li
                          key={`${event.check_id}:${event.finished_at}:${index}`}
                          className="grid gap-3 px-4 py-4 sm:grid-cols-[100px_minmax(0,1fr)_auto] sm:px-5"
                        >
                          <div>
                            <StatusBadge
                              status={
                                event.outcome === "resolved"
                                  ? "succeeded"
                                  : "not_requested"
                              }
                              label={
                                event.outcome === "resolved"
                                  ? "已恢复"
                                  : "已停止检查"
                              }
                            />
                          </div>
                          <div className="min-w-0">
                            <div className="break-words text-sm font-medium">
                              {t(event.title)}
                            </div>
                            <p className="mt-1 break-words text-xs text-muted-foreground">
                              {resourceText(event)}
                            </p>
                            <p className="mt-2 break-words text-xs text-muted-foreground">
                              {formatDateTime(event.started_at)} →{" "}
                              {formatDateTime(event.finished_at)}
                            </p>
                            <p className="mt-1 text-xs text-muted-foreground">
                              {event.outcome === "resolved"
                                ? t("后续检查已通过")
                                : t("检查对象已移除或停用，不代表故障恢复")}
                            </p>
                          </div>
                          <Button variant="ghost" size="sm" asChild>
                            <Link to={event.action_url}>
                              {t("查看对象")}
                              <ArrowUpRight />
                            </Link>
                          </Button>
                        </li>
                      ))}
                    </ul>
                  </Panel>
                ) : (
                  <EmptyState
                    icon={CheckCheck}
                    title={t("暂无恢复记录")}
                    description={t(
                      "异常检查恢复正常后会自动记录；停用或移除会单独标记。",
                    )}
                  />
                )}
              </TabsContent>
            </Tabs>
            <Dialog
              open={Boolean(selected)}
              onOpenChange={(open) => {
                if (!open) setSelectedID(undefined);
              }}
            >
              <DialogContent className="max-h-[85dvh] overflow-y-auto sm:max-w-2xl">
                {selected ? (
                  <>
                    <DialogHeader>
                      <DialogTitle>{t(selected.title)}</DialogTitle>
                      <DialogDescription>
                        {resourceText(selected) ||
                          t(
                            categoryNames[selected.category] ??
                              selected.category,
                          )}
                      </DialogDescription>
                    </DialogHeader>
                    <div className="flex flex-wrap items-center gap-2">
                      <HealthBadge state={selected.state} />
                      <span className="text-sm">{t(selected.summary)}</span>
                    </div>
                    <div className="rounded-md border bg-muted/30 p-4">
                      <p className="text-xs font-medium">{t("影响说明")}</p>
                      <p className="mt-2 text-sm leading-relaxed text-muted-foreground">
                        {t(selected.impact)}
                      </p>
                    </div>
                    <dl className="grid gap-4 sm:grid-cols-2">
                      <HealthDatum
                        label={t("本轮首次发现")}
                        value={formatDateTime(selected.since)}
                      />
                      <HealthDatum
                        label={t("来源数据时间")}
                        value={
                          selected.observed_at
                            ? formatDateTime(selected.observed_at)
                            : t("尚无记录")
                        }
                      />
                    </dl>
                    <div>
                      <h3 className="mb-3 text-sm font-medium">
                        {t("判断证据")}
                      </h3>
                      <dl className="divide-y rounded-md border">
                        {selected.evidence.map((e, index) => (
                          <div
                            key={index}
                            className="grid gap-1 px-3 py-2.5 text-sm sm:grid-cols-[150px_minmax(0,1fr)]"
                          >
                            <dt className="text-muted-foreground">
                              {t(e.label)}
                            </dt>
                            <dd className="min-w-0 whitespace-pre-wrap break-all">
                              {healthEvidenceValue(e.value)}
                            </dd>
                          </div>
                        ))}
                      </dl>
                    </div>
                    <div className="flex justify-end">
                      <Button asChild>
                        <Link to={selected.action_url}>
                          {t(selected.action_label)}
                          <ArrowUpRight />
                        </Link>
                      </Button>
                    </div>
                  </>
                ) : null}
              </DialogContent>
            </Dialog>
          </>
        ) : null}
      </PageBody>
    </>
  );
}

function healthEvidenceValue(value: string) {
  if (
    /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(
      value,
    ) &&
    Number.isFinite(Date.parse(value))
  ) {
    return formatDateTime(value);
  }
  return t(value) || "—";
}

function HealthDatum({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="mt-1 break-words text-sm">{value}</dd>
    </div>
  );
}
function HealthFilter({
  id,
  label,
  value,
  onChange,
  options,
}: {
  id: string;
  label: string;
  value: string;
  onChange: (value: string) => void;
  options: { value: string; label: string }[];
}) {
  return (
    <div className="grid min-w-0 gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      <Select value={value} onValueChange={onChange}>
        <SelectTrigger id={id} className="w-full min-w-0">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {options.map((option) => (
            <SelectItem key={option.value} value={option.value}>
              {option.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}
