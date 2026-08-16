import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Activity,
  CheckCircle2,
  CircleAlert,
  Clock3,
  Globe2,
  History,
  LoaderCircle,
  RefreshCw,
  Rocket,
  RotateCcw,
  Search,
  Server,
  Waypoints,
} from "lucide-react";
import { useMemo, useState, type ComponentType } from "react";
import { Link, useSearchParams } from "react-router";
import { toast } from "sonner";

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
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api, errorMessage } from "@/lib/api";
import { formatDateTime, formatNumber } from "@/lib/format";
import { t, useI18n } from "@/lib/i18n";
import type {
  DeploymentTask,
  PublishHistoryOverview,
  PublishNodeResult,
  PublishOverview,
  PublishOverviewNode,
  PublishSiteOverview,
} from "@/lib/types";
import { cn } from "@/lib/utils";

type PublishTab = "tasks" | "history";
type PublishFilter = "all" | "active" | "attention" | "pending";

const activeStatuses = new Set(["queued", "dispatching", "applying"]);
const attentionStatuses = new Set(["partial", "failed"]);
const lateConfirmationWindow = 2 * 60 * 1_000;

export function PublishPage() {
  useI18n();
  const queryClient = useQueryClient();
  const [searchParams, setSearchParams] = useSearchParams();
  const [filter, setFilter] = useState<PublishFilter>("all");
  const [search, setSearch] = useState("");
  const [historySearch, setHistorySearch] = useState("");
  const [historyID, setHistoryID] = useState<string | null>(null);
  const tab: PublishTab =
    searchParams.get("tab") === "history" ? "history" : "tasks";

  const query = useQuery({
    queryKey: ["publish-overview"],
    queryFn: () => api<PublishOverview>("/api/publish"),
    refetchInterval: (current) => {
      const data = current.state.data;
      if (!data) return 20_000;
      if (data.sites.some((site) => taskActive(site.task))) return 2_000;
      const now = Date.now();
      return data.sites.some((site) => {
        if (!site.task || !attentionStatuses.has(site.task.status))
          return false;
        const completedAt = Date.parse(
          site.task.deadline_at ?? site.task.updated_at,
        );
        return (
          Number.isFinite(completedAt) &&
          now <= completedAt + lateConfirmationWindow
        );
      })
        ? 2_000
        : 20_000;
    },
  });

  const sites = useMemo(
    () =>
      [...(query.data?.sites ?? [])].sort((left, right) => {
        const priority = sitePriority(left) - sitePriority(right);
        return priority || left.site_name.localeCompare(right.site_name);
      }),
    [query.data?.sites],
  );
  const filteredSites = useMemo(() => {
    const needle = search.trim().toLocaleLowerCase();
    return sites.filter((site) => {
      const presentation = sitePresentation(site);
      if (filter === "active" && !taskActive(site.task)) return false;
      if (filter === "attention" && presentation.group !== "attention")
        return false;
      if (filter === "pending" && presentation.group !== "pending")
        return false;
      if (!needle) return true;
      return [site.site_name, site.site_id, ...site.domains].some((value) =>
        value.toLocaleLowerCase().includes(needle),
      );
    });
  }, [filter, search, sites]);
  const requestedSiteID = searchParams.get("site_id");
  const selectedSite =
    filteredSites.find((site) => site.site_id === requestedSiteID) ??
    filteredSites[0];

  const filteredHistory = useMemo(() => {
    const needle = historySearch.trim().toLocaleLowerCase();
    if (!needle) return query.data?.history ?? [];
    return (query.data?.history ?? []).filter((item) =>
      [item.site_name, item.site_id, item.task.id, ...item.domains].some(
        (value) => value.toLocaleLowerCase().includes(needle),
      ),
    );
  }, [historySearch, query.data?.history]);
  const selectedHistory =
    filteredHistory.find((item) => item.task.id === historyID) ??
    filteredHistory[0];

  const action = useMutation({
    mutationFn: ({ siteID, retry }: { siteID: string; retry: boolean }) =>
      api<DeploymentTask>(
        `/api/sites/${encodeURIComponent(siteID)}/publish${retry ? "/retry" : ""}`,
        { method: "POST" },
      ),
    onSuccess: async (_, input) => {
      toast.success(
        input.retry ? t("失败节点重试已启动") : t("站点发布已启动"),
      );
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["publish-overview"] }),
        queryClient.invalidateQueries({ queryKey: ["sites"] }),
        queryClient.invalidateQueries({
          queryKey: ["site-publish", input.siteID],
        }),
      ]);
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  const activeCount = sites.filter((site) => taskActive(site.task)).length;
  const attentionCount = sites.filter(
    (site) => sitePresentation(site).group === "attention",
  ).length;
  const pendingCount = sites.filter(
    (site) => sitePresentation(site).group === "pending",
  ).length;
  const ipv6Count = sites.filter((site) => site.ipv6_enabled).length;

  function selectSite(siteID: string) {
    const next = new URLSearchParams(searchParams);
    next.set("site_id", siteID);
    next.delete("tab");
    setSearchParams(next, { replace: true });
  }

  function changeTab(value: string) {
    const next = new URLSearchParams(searchParams);
    if (value === "history") next.set("tab", "history");
    else next.delete("tab");
    setSearchParams(next, { replace: true });
  }

  return (
    <>
      <PageHeader
        title={t("发布")}
        description={t("站点配置分发、节点确认与 DNS 入池状态")}
        actions={
          <Button
            variant="outline"
            size="icon"
            aria-label={t("刷新发布状态")}
            onClick={() => void query.refetch()}
          >
            <RefreshCw
              className={query.isFetching ? "animate-spin" : undefined}
            />
          </Button>
        }
      />
      <PageBody>
        {query.isLoading ? <PageLoading /> : null}
        {query.error ? <PageError error={query.error} /> : null}
        {query.data ? (
          sites.length ? (
            <>
              <div className="grid grid-cols-2 gap-px overflow-hidden rounded-lg border bg-border lg:grid-cols-4">
                <OverviewMetric
                  icon={Clock3}
                  label={t("待发布配置")}
                  value={pendingCount}
                />
                <OverviewMetric
                  icon={Activity}
                  label={t("执行中")}
                  value={activeCount}
                />
                <OverviewMetric
                  icon={CircleAlert}
                  label={t("需处理")}
                  value={attentionCount}
                  alert={attentionCount > 0}
                />
                <OverviewMetric
                  icon={Globe2}
                  label={t("IPv6 站点")}
                  value={ipv6Count}
                />
              </div>

              <Tabs value={tab} onValueChange={changeTab}>
                <TabsList aria-label={t("发布视图")}>
                  <TabsTrigger value="tasks">
                    <Rocket />
                    {t("发布任务")}
                  </TabsTrigger>
                  <TabsTrigger value="history">
                    <History />
                    {t("变更记录")}
                  </TabsTrigger>
                </TabsList>
                <TabsContent value="tasks" className="space-y-4">
                  <div className="flex flex-col gap-2 sm:flex-row">
                    <div className="relative min-w-0 flex-1 sm:max-w-sm">
                      <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                      <Input
                        value={search}
                        onChange={(event) => setSearch(event.target.value)}
                        placeholder={t("搜索站点或域名")}
                        aria-label={t("搜索发布任务")}
                        className="pl-9"
                      />
                    </div>
                    <Select
                      value={filter}
                      onValueChange={(value) =>
                        setFilter(value as PublishFilter)
                      }
                    >
                      <SelectTrigger
                        className="w-full sm:w-40"
                        aria-label={t("筛选发布状态")}
                      >
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="all">{t("全部站点")}</SelectItem>
                        <SelectItem value="active">{t("执行中")}</SelectItem>
                        <SelectItem value="attention">{t("需处理")}</SelectItem>
                        <SelectItem value="pending">{t("待发布")}</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  {filteredSites.length ? (
                    <div className="grid min-w-0 gap-4 xl:grid-cols-[21rem_minmax(0,1fr)]">
                      <Panel className="self-start">
                        <div className="border-b px-4 py-3">
                          <div className="text-sm font-medium">
                            {t("站点任务")}
                          </div>
                          <div className="mt-0.5 text-xs text-muted-foreground">
                            {t("{value0} 个站点", {
                              value0: filteredSites.length,
                            })}
                          </div>
                        </div>
                        <div className="max-h-[44rem] divide-y overflow-auto">
                          {filteredSites.map((site) => (
                            <SiteTaskButton
                              key={site.site_id}
                              site={site}
                              selected={site.site_id === selectedSite?.site_id}
                              onSelect={() => selectSite(site.site_id)}
                            />
                          ))}
                        </div>
                      </Panel>
                      {selectedSite ? (
                        <PublishDetail
                          site={selectedSite}
                          pending={
                            action.isPending &&
                            action.variables?.siteID === selectedSite.site_id
                          }
                          onPublish={() =>
                            action.mutate({
                              siteID: selectedSite.site_id,
                              retry: false,
                            })
                          }
                          onRetry={() =>
                            action.mutate({
                              siteID: selectedSite.site_id,
                              retry: true,
                            })
                          }
                        />
                      ) : null}
                    </div>
                  ) : (
                    <EmptyState
                      title={t("没有匹配的发布任务")}
                      description={t("调整筛选条件查看其他站点")}
                    />
                  )}
                </TabsContent>
                <TabsContent value="history" className="space-y-4">
                  <div className="relative max-w-sm">
                    <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                    <Input
                      value={historySearch}
                      onChange={(event) => setHistorySearch(event.target.value)}
                      placeholder={t("搜索站点或任务 ID")}
                      aria-label={t("搜索变更记录")}
                      className="pl-9"
                    />
                  </div>
                  {filteredHistory.length ? (
                    <div className="grid min-w-0 gap-4 xl:grid-cols-[minmax(0,1fr)_24rem]">
                      <HistoryList
                        entries={filteredHistory}
                        selectedID={selectedHistory?.task.id}
                        onSelect={setHistoryID}
                      />
                      {selectedHistory ? (
                        <HistoryDetail entry={selectedHistory} />
                      ) : null}
                    </div>
                  ) : (
                    <EmptyState
                      title={t("暂无发布记录")}
                      description={t("站点发布后将在这里保留任务与节点结果")}
                    />
                  )}
                </TabsContent>
              </Tabs>
            </>
          ) : (
            <EmptyState
              title={t("暂无站点")}
              description={t("创建站点后可在这里统一管理发布")}
              action={
                <Button asChild>
                  <Link to="/sites/new">{t("添加站点")}</Link>
                </Button>
              }
            />
          )
        ) : null}
      </PageBody>
    </>
  );
}

function OverviewMetric({
  icon: Icon,
  label,
  value,
  alert = false,
}: {
  icon: ComponentType<{ className?: string; "aria-hidden"?: boolean }>;
  label: string;
  value: number;
  alert?: boolean;
}) {
  return (
    <div className="flex min-h-24 items-center gap-3 bg-card px-4 py-4">
      <div
        className={cn(
          "flex size-9 shrink-0 items-center justify-center rounded-md border bg-background text-muted-foreground",
          alert && "border-destructive/30 text-destructive",
        )}
      >
        <Icon className="size-4" aria-hidden />
      </div>
      <div className="min-w-0">
        <div className="text-xs text-muted-foreground">{label}</div>
        <div className="mt-0.5 text-xl font-semibold tabular-nums">
          {formatNumber(value)}
        </div>
      </div>
    </div>
  );
}

function SiteTaskButton({
  site,
  selected,
  onSelect,
}: {
  site: PublishSiteOverview;
  selected: boolean;
  onSelect: () => void;
}) {
  const presentation = sitePresentation(site);
  const failed = site.nodes.filter((node) =>
    ["failed", "timed_out"].includes(node.configuration_status),
  ).length;
  return (
    <button
      type="button"
      className={cn(
        "block w-full border-l-2 border-l-transparent px-4 py-3 text-left transition-colors hover:bg-muted/45 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring",
        selected && "border-l-primary bg-muted/55",
      )}
      onClick={onSelect}
    >
      <span className="flex items-start justify-between gap-3">
        <span className="min-w-0">
          <span className="block truncate text-sm font-medium">
            {site.site_name}
          </span>
          <span className="mt-0.5 block truncate text-xs text-muted-foreground">
            {site.domains.join(", ") || t("无 HTTP 域名")}
          </span>
        </span>
        <StatusBadge status={presentation.status} label={presentation.label} />
      </span>
      <span className="mt-2 flex items-center justify-between gap-3 text-xs text-muted-foreground">
        <span>V{formatNumber(site.config_version)}</span>
        <span>
          {failed
            ? t("{value0} 个节点异常", { value0: failed })
            : t("{value0} 个节点", { value0: currentNodes(site).length })}
        </span>
      </span>
    </button>
  );
}

function PublishDetail({
  site,
  pending,
  onPublish,
  onRetry,
}: {
  site: PublishSiteOverview;
  pending: boolean;
  onPublish: () => void;
  onRetry: () => void;
}) {
  const presentation = sitePresentation(site);
  const nodes = currentNodes(site);
  const failedNodes = site.nodes.filter((node) =>
    ["failed", "timed_out"].includes(node.configuration_status),
  );
  const configured = nodes.filter(
    (node) => node.configuration_status === "succeeded",
  ).length;
  const ipv4Pool = dnsPool(nodes, "ipv4_dns_eligible");
  const ipv6Nodes = nodes.filter((node) => Boolean(node.public_ipv6));
  const ipv6Pool = dnsPool(nodes, "ipv6_dns_eligible");
  const ipv4Checked = nodes.filter((node) => node.ipv4_last_checked_at).length;
  const ipv6Checked = ipv6Nodes.filter(
    (node) => node.ipv6_last_checked_at,
  ).length;
  const onlyIPv4 = nodes.length - ipv6Nodes.length;
  const http3Capable = nodes.filter((node) =>
    node.capabilities.includes("http3_v1"),
  ).length;
  const tcpCapable = nodes.filter((node) =>
    node.capabilities.includes("tcp_stream_v1"),
  ).length;
  const publishActive = taskActive(site.task);
  const ipv4Status = !site.published
    ? "pending"
    : ipv4Pool.length
      ? "succeeded"
      : ipv4Checked
        ? "failed"
        : "queued";
  const ipv6Status = !site.ipv6_enabled
    ? "not_requested"
    : !ipv6Nodes.length
      ? "unsupported"
      : ipv6Pool.length
        ? "succeeded"
        : ipv6Checked
          ? "failed"
          : "queued";
  const dnsStatus = !site.published
    ? "pending"
    : !ipv4Pool.length
      ? ipv4Checked
        ? "failed"
        : "queued"
      : site.ipv6_enabled && !ipv6Pool.length
        ? "partial"
        : "succeeded";

  return (
    <Panel className="min-w-0">
      <div className="flex flex-col gap-4 border-b px-4 py-4 sm:flex-row sm:items-start sm:justify-between sm:px-5">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <h2 className="truncate text-base font-semibold">
              {site.site_name}
            </h2>
            <StatusBadge
              status={presentation.status}
              label={presentation.label}
            />
          </div>
          <div className="mt-1 truncate text-xs text-muted-foreground">
            {site.domains.join(", ") || t("无 HTTP 域名")} · V
            {formatNumber(site.config_version)}
          </div>
          {site.task ? (
            <div className="mt-1 text-xs text-muted-foreground">
              {formatDateTime(site.task.updated_at)} ·{" "}
              {site.task.id.slice(0, 8)}
            </div>
          ) : null}
        </div>
        <div className="flex flex-wrap gap-2">
          {failedNodes.length && site.published && !publishActive ? (
            <Button variant="outline" disabled={pending} onClick={onRetry}>
              {pending ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <RotateCcw />
              )}
              {t("重试失败节点")}
            </Button>
          ) : null}
          <Button
            disabled={site.deleting || pending || publishActive}
            onClick={onPublish}
          >
            {pending ? <LoaderCircle className="animate-spin" /> : <Rocket />}
            {site.published ? t("重新发布") : t("发布站点")}
          </Button>
          <Button asChild variant="outline">
            <Link to={`/sites/${encodeURIComponent(site.site_id)}`}>
              <Waypoints />
              {t("站点配置")}
            </Link>
          </Button>
        </div>
      </div>

      <PublishTimeline task={site.task} dnsStatus={dnsStatus} />

      <div className="grid gap-px border-y bg-border sm:grid-cols-2 xl:grid-cols-4">
        <LayerMetric
          icon={Server}
          label={t("配置应用")}
          value={`${formatNumber(configured)} / ${formatNumber(nodes.length)}`}
          detail={t("已应用当前节点配置")}
          status={presentation.status}
        />
        <LayerMetric
          icon={Globe2}
          label="IPv4"
          value={t("{value0} 个入池节点", { value0: ipv4Pool.length })}
          detail={
            ipv4Checked
              ? t("{value0} 个节点已检测", { value0: ipv4Checked })
              : t("等待健康检查")
          }
          status={ipv4Status}
        />
        <LayerMetric
          icon={Globe2}
          label="IPv6"
          value={
            site.ipv6_enabled
              ? t("{value0} 个入池节点", { value0: ipv6Pool.length })
              : t("未启用")
          }
          detail={
            site.ipv6_enabled
              ? t("{value0} 个节点仅参与 IPv4", { value0: onlyIPv4 })
              : t("不创建 AAAA 记录")
          }
          status={ipv6Status}
        />
        <LayerMetric
          icon={Activity}
          label={t("DNS 池")}
          value={`A ${formatNumber(ipv4Pool.length)} · AAAA ${formatNumber(ipv6Pool.length)}`}
          detail={
            site.ipv6_enabled && !ipv6Nodes.length
              ? t("IPv6 节点缺失，不影响 IPv4 发布")
              : t("主节点不可用时使用备用节点")
          }
          status={dnsStatus}
        />
      </div>

      <div className="grid gap-px border-b bg-border sm:grid-cols-3">
        <CapabilityItem
          label={t("IPv6 地址")}
          enabled={site.ipv6_enabled}
          capable={ipv6Nodes.length}
          total={nodes.length}
        />
        <CapabilityItem
          label="HTTP/3"
          enabled={site.http3_enabled}
          capable={http3Capable}
          total={nodes.length}
        />
        <CapabilityItem
          label="TCP Stream"
          enabled={site.tcp_enabled}
          capable={tcpCapable}
          total={nodes.length}
        />
      </div>

      <div className="border-b px-4 py-3 sm:px-5">
        <h3 className="text-sm font-medium">{t("节点执行结果")}</h3>
        <p className="mt-0.5 text-xs text-muted-foreground">
          {t("配置确认与双栈 DNS 资格")}
        </p>
      </div>
      {site.nodes.length ? (
        <NodeResults site={site} />
      ) : (
        <div className="px-5 py-10 text-center text-sm text-muted-foreground">
          {t("当前配置未分配边缘节点")}
        </div>
      )}
      {site.task?.detail ? (
        <div className="border-t bg-muted/25 px-4 py-3 text-xs text-muted-foreground sm:px-5">
          {site.task.detail}
        </div>
      ) : null}
    </Panel>
  );
}

function PublishTimeline({
  task,
  dnsStatus,
}: {
  task: DeploymentTask | null;
  dnsStatus: string;
}) {
  const terminal = Boolean(task && !taskActive(task));
  const steps = [
    {
      label: t("构建配置"),
      status: task
        ? task.status === "queued"
          ? "applying"
          : "succeeded"
        : "pending",
    },
    {
      label: t("分发配置"),
      status: !task
        ? "pending"
        : task.status === "queued"
          ? "pending"
          : task.status === "dispatching"
            ? "applying"
            : "succeeded",
    },
    {
      label: t("节点确认"),
      status: !task
        ? "pending"
        : terminal
          ? task.status
          : task.status === "applying"
            ? "applying"
            : "pending",
    },
    {
      label: t("DNS 入池"),
      status: terminal ? dnsStatus : "pending",
    },
  ];
  return (
    <div
      data-slot="publish-timeline"
      className="grid gap-px bg-border sm:grid-cols-4"
    >
      {steps.map((step, index) => (
        <div
          key={step.label}
          className="flex items-center gap-3 bg-card px-4 py-3"
        >
          <span
            className={cn(
              "flex size-6 shrink-0 items-center justify-center rounded-full border bg-background text-[0.6875rem] font-medium text-muted-foreground",
              step.status === "succeeded" &&
                "border-success/40 bg-success/10 text-success",
              ["applying", "dispatching"].includes(step.status) &&
                "border-info/40 bg-info/10 text-info",
              ["partial", "failed"].includes(step.status) &&
                "border-destructive/30 bg-destructive/10 text-destructive",
            )}
          >
            {step.status === "succeeded" ? (
              <CheckCircle2 className="size-3.5" />
            ) : (
              index + 1
            )}
          </span>
          <span className="min-w-0 truncate text-xs font-medium">
            {step.label}
          </span>
        </div>
      ))}
    </div>
  );
}

function LayerMetric({
  icon: Icon,
  label,
  value,
  detail,
  status,
}: {
  icon: ComponentType<{ className?: string }>;
  label: string;
  value: string;
  detail: string;
  status: string;
}) {
  return (
    <div className="min-w-0 bg-card px-4 py-4">
      <div className="flex items-center justify-between gap-2">
        <span className="flex min-w-0 items-center gap-2 text-xs text-muted-foreground">
          <Icon className="size-4 shrink-0" />
          <span className="truncate">{label}</span>
        </span>
        <StatusBadge status={status} />
      </div>
      <div className="mt-3 text-base font-semibold tabular-nums">{value}</div>
      <div className="mt-1 text-xs leading-5 text-muted-foreground">
        {detail}
      </div>
    </div>
  );
}

function CapabilityItem({
  label,
  enabled,
  capable,
  total,
}: {
  label: string;
  enabled: boolean;
  capable: number;
  total: number;
}) {
  return (
    <div className="flex items-center justify-between gap-3 bg-card px-4 py-3 text-xs">
      <span className="text-muted-foreground">{label}</span>
      {enabled ? (
        <span className="font-medium tabular-nums">
          {formatNumber(capable)} / {formatNumber(total)}
        </span>
      ) : (
        <span className="text-muted-foreground">{t("未启用")}</span>
      )}
    </div>
  );
}

function NodeResults({ site }: { site: PublishSiteOverview }) {
  return (
    <>
      <div className="hidden overflow-x-auto lg:block">
        <Table className="min-w-[46rem]">
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">{t("节点")}</TableHead>
              <TableHead>{t("配置版本")}</TableHead>
              <TableHead>{t("配置")}</TableHead>
              <TableHead>IPv4</TableHead>
              <TableHead>IPv6</TableHead>
              <TableHead className="pr-5">{t("诊断")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {site.nodes.map((node) => (
              <TableRow key={node.node_id}>
                <TableCell className="pl-5">
                  <NodeIdentity node={node} />
                </TableCell>
                <TableCell className="whitespace-nowrap font-mono text-xs tabular-nums">
                  {formatNumber(node.applied_version)} /{" "}
                  {formatNumber(node.desired_version)}
                </TableCell>
                <TableCell>
                  <ConfigurationBadge node={node} />
                </TableCell>
                <TableCell>
                  <AddressBadge node={node} family="ipv4" enabled />
                </TableCell>
                <TableCell>
                  <AddressBadge
                    node={node}
                    family="ipv6"
                    enabled={site.ipv6_enabled}
                  />
                </TableCell>
                <TableCell className="max-w-64 pr-5 text-xs text-muted-foreground">
                  <NodeDiagnostic node={node} ipv6Enabled={site.ipv6_enabled} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      <div className="divide-y lg:hidden">
        {site.nodes.map((node) => (
          <div key={node.node_id} className="space-y-3 px-4 py-4">
            <div className="flex items-start justify-between gap-3">
              <NodeIdentity node={node} />
              <ConfigurationBadge node={node} />
            </div>
            <div className="grid grid-cols-2 gap-3 text-xs">
              <div>
                <div className="mb-1 text-muted-foreground">IPv4</div>
                <AddressBadge node={node} family="ipv4" enabled />
              </div>
              <div>
                <div className="mb-1 text-muted-foreground">IPv6</div>
                <AddressBadge
                  node={node}
                  family="ipv6"
                  enabled={site.ipv6_enabled}
                />
              </div>
            </div>
            <div className="text-xs leading-5 text-muted-foreground">
              <NodeDiagnostic node={node} ipv6Enabled={site.ipv6_enabled} />
            </div>
          </div>
        ))}
      </div>
    </>
  );
}

function NodeIdentity({ node }: { node: PublishOverviewNode }) {
  const runtime = nodeRuntimeLabel(node);
  return (
    <div className="min-w-0">
      <div className="truncate text-sm font-medium">{node.node_name}</div>
      <div className="mt-0.5 text-xs text-muted-foreground">
        {roleLabel(node.role)} · {node.public_ipv4 || "--"}
      </div>
      <div className="mt-0.5 font-mono text-[0.6875rem] text-muted-foreground">
        <div className="truncate" title={runtime.agent.full}>
          Agent {runtime.agent.short}
        </div>
        <div className="truncate" title={runtime.nginx.full}>
          Nginx {runtime.nginx.short}
        </div>
      </div>
    </div>
  );
}

function ConfigurationBadge({ node }: { node: PublishOverviewNode }) {
  const label =
    node.configuration_status === "not_targeted" ? t("未纳入") : undefined;
  return <StatusBadge status={node.configuration_status} label={label} />;
}

function AddressBadge({
  node,
  family,
  enabled,
}: {
  node: PublishOverviewNode;
  family: "ipv4" | "ipv6";
  enabled: boolean;
}) {
  if (!enabled)
    return <StatusBadge status="not_requested" label={t("未启用")} />;
  if (family === "ipv6" && !node.public_ipv6)
    return <StatusBadge status="unsupported" label={t("仅 IPv4")} />;
  const eligible =
    family === "ipv4" ? node.ipv4_dns_eligible : node.ipv6_dns_eligible;
  const checkedAt =
    family === "ipv4" ? node.ipv4_last_checked_at : node.ipv6_last_checked_at;
  if (eligible) return <StatusBadge status="succeeded" label={t("已入池")} />;
  if (!checkedAt) return <StatusBadge status="queued" label={t("待检测")} />;
  return <StatusBadge status="failed" label={t("未入池")} />;
}

function NodeDiagnostic({
  node,
  ipv6Enabled,
}: {
  node: PublishOverviewNode;
  ipv6Enabled: boolean;
}) {
  const failed = ["failed", "timed_out"].includes(node.configuration_status);
  const detail =
    (failed ? node.detail || errorCodeLabel(node.error_code) : "") ||
    node.node_last_error ||
    driftReasonLabel(node) ||
    node.ipv4_last_error ||
    (ipv6Enabled && node.public_ipv6 ? node.ipv6_last_error : "");
  return detail || t("无异常");
}

function nodeRuntimeLabel(node: PublishOverviewNode) {
  return {
    agent: runtimeArtifact(node.agent_version, node.agent_sha256),
    nginx: runtimeArtifact(node.nginx_version, node.nginx_sha256),
  };
}

function runtimeArtifact(version?: string, sha256?: string) {
  const label = version ? `v${version}` : "--";
  if (!sha256) return { short: label, full: label };
  return {
    short: `${label}@${sha256.slice(0, 8)}`,
    full: `${label}@${sha256}`,
  };
}

function driftReasonLabel(node: PublishOverviewNode) {
  switch (node.drift_reason) {
    case "node_inactive":
      return t("节点未处于活动状态");
    case "desired_state_missing":
      return t("尚未生成节点目标配置");
    case "version_behind":
      return t("等待节点应用目标版本 V{value0}", {
        value0: node.desired_version,
      });
    case "publication_active":
      return t("节点配置发布仍在执行");
    default:
      return "";
  }
}

function HistoryList({
  entries,
  selectedID,
  onSelect,
}: {
  entries: PublishHistoryOverview[];
  selectedID?: string;
  onSelect: (taskID: string) => void;
}) {
  return (
    <Panel className="min-w-0 self-start">
      <div className="overflow-x-auto">
        <Table className="min-w-[42rem]">
          <TableHeader>
            <TableRow>
              <TableHead className="pl-5">{t("时间")}</TableHead>
              <TableHead>{t("站点")}</TableHead>
              <TableHead>{t("结果")}</TableHead>
              <TableHead>{t("节点")}</TableHead>
              <TableHead className="pr-5">{t("任务 ID")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {entries.map((entry) => {
              const counts = historyCounts(entry.nodes);
              return (
                <TableRow
                  key={entry.task.id}
                  data-state={
                    selectedID === entry.task.id ? "selected" : undefined
                  }
                >
                  <TableCell className="whitespace-nowrap pl-5 text-xs text-muted-foreground">
                    {formatDateTime(entry.task.created_at)}
                  </TableCell>
                  <TableCell>
                    <Button
                      variant="link"
                      className="h-auto max-w-56 justify-start truncate p-0 text-foreground"
                      onClick={() => onSelect(entry.task.id)}
                    >
                      {entry.site_name}
                    </Button>
                  </TableCell>
                  <TableCell>
                    <StatusBadge status={entry.task.status} />
                  </TableCell>
                  <TableCell className="whitespace-nowrap text-xs tabular-nums">
                    {counts.succeeded} / {counts.total}
                  </TableCell>
                  <TableCell className="pr-5 font-mono text-xs text-muted-foreground">
                    {entry.task.id.slice(0, 8)}
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      </div>
    </Panel>
  );
}

function HistoryDetail({ entry }: { entry: PublishHistoryOverview }) {
  const counts = historyCounts(entry.nodes);
  return (
    <Panel className="min-w-0 self-start">
      <div className="border-b px-4 py-4">
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <h2 className="truncate text-sm font-semibold">
              {entry.site_name}
            </h2>
            <div className="mt-1 font-mono text-xs text-muted-foreground">
              {entry.task.id}
            </div>
          </div>
          <StatusBadge status={entry.task.status} />
        </div>
        <div className="mt-3 grid grid-cols-3 gap-2 text-xs">
          <HistoryCount label={t("成功")} value={counts.succeeded} />
          <HistoryCount label={t("失败")} value={counts.failed} />
          <HistoryCount label={t("目标节点")} value={counts.total} />
        </div>
      </div>
      {entry.nodes.length ? (
        <div className="divide-y">
          {entry.nodes.map((node) => (
            <div key={node.node_id} className="px-4 py-3">
              <div className="flex items-start justify-between gap-3">
                <div className="min-w-0">
                  <div className="truncate text-sm font-medium">
                    {node.node_name || node.node_id}
                  </div>
                  <div className="mt-0.5 text-xs text-muted-foreground">
                    {t("目标版本")} {formatNumber(node.target_version)}
                  </div>
                </div>
                <StatusBadge status={node.status} />
              </div>
              {node.detail || node.error_code ? (
                <div className="mt-2 text-xs leading-5 text-muted-foreground">
                  {node.detail || errorCodeLabel(node.error_code)}
                </div>
              ) : null}
            </div>
          ))}
        </div>
      ) : (
        <div className="px-4 py-8 text-center text-xs text-muted-foreground">
          {t("本次发布未生成节点配置变更")}
        </div>
      )}
      <div className="border-t bg-muted/25 px-4 py-3 text-xs leading-5 text-muted-foreground">
        {entry.task.detail || t("无任务详情")}
      </div>
    </Panel>
  );
}

function HistoryCount({ label, value }: { label: string; value: number }) {
  return (
    <div className="rounded-md border bg-background px-2 py-2 text-center">
      <div className="font-semibold tabular-nums">{formatNumber(value)}</div>
      <div className="mt-0.5 text-muted-foreground">{label}</div>
    </div>
  );
}

function taskActive(task?: DeploymentTask | null) {
  return Boolean(task && activeStatuses.has(task.status));
}

function sitePresentation(site: PublishSiteOverview): {
  status: string;
  label?: string;
  group: "normal" | "active" | "attention" | "pending";
} {
  if (site.deleting)
    return { status: "applying", label: t("删除中"), group: "active" };
  if (!site.enabled)
    return { status: "unsupported", label: t("已停用"), group: "normal" };
  if (taskActive(site.task))
    return { status: site.task!.status, group: "active" };
  if (!site.published)
    return { status: "pending", label: t("待发布"), group: "pending" };
  if (site.task && attentionStatuses.has(site.task.status))
    return { status: site.task.status, group: "attention" };
  return { status: "succeeded", label: t("已发布"), group: "normal" };
}

function sitePriority(site: PublishSiteOverview) {
  const group = sitePresentation(site).group;
  if (group === "active") return 0;
  if (group === "attention") return 1;
  if (group === "pending") return 2;
  return 3;
}

function currentNodes(site: PublishSiteOverview) {
  return site.nodes.filter((node) => node.role !== "removed");
}

function dnsPool(
  nodes: PublishOverviewNode[],
  field: "ipv4_dns_eligible" | "ipv6_dns_eligible",
) {
  const primary = nodes.filter(
    (node) => node.role === "primary" && node[field],
  );
  if (primary.length) return primary;
  return nodes.filter((node) => node.role === "backup" && node[field]);
}

function roleLabel(role: PublishOverviewNode["role"]) {
  if (role === "primary") return t("主节点");
  if (role === "backup") return t("备用节点");
  return t("移出节点");
}

function errorCodeLabel(code?: string) {
  const labels: Record<string, string> = {
    confirmation_timeout: t("边缘确认超时"),
    nginx_config_test_failed: t("Nginx 配置校验失败"),
    port_conflict: t("端口冲突"),
  };
  return code ? (labels[code] ?? code) : "";
}

function historyCounts(nodes: PublishNodeResult[]) {
  return nodes.reduce(
    (counts, node) => {
      counts.total += 1;
      if (node.status === "succeeded") counts.succeeded += 1;
      if (["failed", "timed_out"].includes(node.status)) counts.failed += 1;
      return counts;
    },
    { total: 0, succeeded: 0, failed: 0 },
  );
}
