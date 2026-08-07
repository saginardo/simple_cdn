import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Activity,
  AlertTriangle,
  DatabaseZap,
  Eye,
  Flame,
  LoaderCircle,
  Plus,
  RefreshCw,
  RotateCcw,
  Search,
  Server,
} from "lucide-react";
import {
  useEffect,
  useMemo,
  useState,
  type FormEvent,
  type ReactNode,
} from "react";
import { useSearchParams } from "react-router";
import { toast } from "sonner";

import { ListPagination } from "@/components/list-pagination";
import {
  EmptyState,
  PageBody,
  PageError,
  PageHeader,
  PageLoading,
  Panel,
} from "@/components/page";
import { StatusBadge } from "@/components/status-badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
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
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { useListPagination } from "@/hooks/use-list-pagination";
import { api, errorMessage } from "@/lib/api";
import { formatDateTime, formatNumber } from "@/lib/format";
import { t, useI18n } from "@/lib/i18n";
import type {
  CacheOperation,
  CacheOperationsOverview,
  CacheSiteOverview,
} from "@/lib/types";

type CacheTab = "operations" | "sites" | "rules";
type CacheScope = "url" | "prefix" | "full";

interface OperationDraft {
  site_id: string;
  scope: CacheScope;
  value: string;
  prewarm: boolean;
  prewarm_paths: string;
  full_confirmation: string;
}

const activeStatuses = new Set(["queued", "applying"]);

export function CachePage() {
  useI18n();
  const queryClient = useQueryClient();
  const [searchParams, setSearchParams] = useSearchParams();
  const initialSiteID = searchParams.get("site_id") ?? "all";
  const initialTab = searchParams.get("tab") as CacheTab | null;
  const [tab, setTab] = useState<CacheTab>(
    initialTab && ["operations", "sites", "rules"].includes(initialTab)
      ? initialTab
      : "operations",
  );
  const [siteFilter, setSiteFilter] = useState(initialSiteID);
  const [statusFilter, setStatusFilter] = useState("all");
  const [search, setSearch] = useState("");
  const [createOpen, setCreateOpen] = useState(false);
  const [detailID, setDetailID] = useState<string | null>(null);
  const [draft, setDraft] = useState<OperationDraft>(() =>
    emptyDraft(initialSiteID === "all" ? "" : initialSiteID),
  );

  const query = useQuery({
    queryKey: ["cache-operations-overview"],
    queryFn: () => api<CacheOperationsOverview>("/api/cache/overview"),
    refetchInterval: (result) =>
      result.state.data?.operations.some((operation) =>
        activeStatuses.has(operation.status),
      )
        ? 3_000
        : 30_000,
  });

  const refresh = async () => {
    await Promise.all([
      queryClient.invalidateQueries({
        queryKey: ["cache-operations-overview"],
      }),
      queryClient.invalidateQueries({ queryKey: ["sites"] }),
    ]);
  };
  const createMutation = useMutation({
    mutationFn: (value: OperationDraft) =>
      api<CacheOperation>("/api/cache/operations", {
        method: "POST",
        body: JSON.stringify({
          site_id: value.site_id,
          scope: value.scope,
          value: value.scope === "full" ? "" : value.value.trim(),
          prewarm: value.prewarm,
          prewarm_paths: splitPaths(value.prewarm_paths),
        }),
      }),
    onSuccess: async (operation) => {
      setCreateOpen(false);
      setDetailID(operation.id);
      toast.success(
        operation.prewarm_paths.length
          ? t("缓存失效与预热已提交")
          : t("缓存失效已提交"),
      );
      await refresh();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const retryMutation = useMutation({
    mutationFn: (operationID: string) =>
      api<CacheOperation>(
        `/api/cache/operations/${encodeURIComponent(operationID)}/retry`,
        { method: "POST" },
      ),
    onSuccess: async (operation) => {
      setDetailID(operation.id);
      toast.success(t("预热重试已提交"));
      await refresh();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  const eligibleSites = useMemo(
    () => (query.data?.sites ?? []).filter((site) => site.cacheable),
    [query.data?.sites],
  );
  useEffect(() => {
    if (draft.site_id || !eligibleSites.length) return;
    setDraft((current) => ({ ...current, site_id: eligibleSites[0].site_id }));
  }, [draft.site_id, eligibleSites]);

  const filteredOperations = useMemo(() => {
    const needle = search.trim().toLocaleLowerCase();
    return (query.data?.operations ?? []).filter((operation) => {
      if (siteFilter !== "all" && operation.site_id !== siteFilter)
        return false;
      if (statusFilter !== "all" && operation.status !== statusFilter)
        return false;
      if (!needle) return true;
      return [
        operation.id,
        operation.site_name,
        operation.target ?? "",
        operation.actor ?? "",
      ].some((value) => value.toLocaleLowerCase().includes(needle));
    });
  }, [query.data?.operations, search, siteFilter, statusFilter]);
  const operationPagination = useListPagination(filteredOperations);
  useEffect(() => {
    operationPagination.setPage(1);
  }, [search, siteFilter, statusFilter]);

  const detailOperation = query.data?.operations.find(
    (operation) => operation.id === detailID,
  );
  const selectedSite = eligibleSites.find(
    (site) => site.site_id === draft.site_id,
  );
  const activeOperationCount =
    query.data?.operations.filter((operation) =>
      activeStatuses.has(operation.status),
    ).length ?? 0;
  const reportingNodes =
    query.data?.sites.reduce(
      (total, site) => total + site.reporting_node_count,
      0,
    ) ?? 0;

  function updateTab(value: string) {
    const next = value as CacheTab;
    setTab(next);
    const params = new URLSearchParams(searchParams);
    if (next === "operations") params.delete("tab");
    else params.set("tab", next);
    setSearchParams(params, { replace: true });
  }

  function updateSiteFilter(value: string) {
    setSiteFilter(value);
    const params = new URLSearchParams(searchParams);
    if (value === "all") params.delete("site_id");
    else params.set("site_id", value);
    setSearchParams(params, { replace: true });
  }

  function openCreate(siteID?: string) {
    const preferredSite =
      siteID ??
      (siteFilter !== "all" ? siteFilter : (eligibleSites[0]?.site_id ?? ""));
    const nextSite = eligibleSites.some(
      (site) => site.site_id === preferredSite,
    )
      ? preferredSite
      : (eligibleSites[0]?.site_id ?? "");
    setDraft(emptyDraft(nextSite));
    setCreateOpen(true);
  }

  return (
    <>
      <PageHeader
        title={t("缓存运维台")}
        description={t("跨站点缓存失效、预热与节点执行结果")}
        actions={
          <Button onClick={() => openCreate()} disabled={!eligibleSites.length}>
            <Plus />
            {t("新建操作")}
          </Button>
        }
      />
      <PageBody>
        {query.isLoading ? <PageLoading /> : null}
        {query.error ? <PageError error={query.error} /> : null}
        {query.data ? (
          <>
            <div className="grid grid-cols-2 border-y sm:grid-cols-4">
              <OverviewStat
                icon={DatabaseZap}
                label={t("可管理站点")}
                value={formatNumber(eligibleSites.length)}
              />
              <OverviewStat
                icon={Activity}
                label={t("执行中")}
                value={formatNumber(activeOperationCount)}
              />
              <OverviewStat
                icon={Flame}
                label={t("活动规则")}
                value={formatNumber(query.data.rules.length)}
              />
              <OverviewStat
                icon={Server}
                label={t("精确上报节点")}
                value={formatNumber(reportingNodes)}
              />
            </div>

            <Tabs value={tab} onValueChange={updateTab}>
              <TabsList
                variant="line"
                className="max-w-full justify-start flex-wrap group-data-horizontal/tabs:h-auto"
              >
                <TabsTrigger value="operations">{t("操作历史")}</TabsTrigger>
                <TabsTrigger value="sites">{t("站点缓存")}</TabsTrigger>
                <TabsTrigger value="rules">{t("活动规则")}</TabsTrigger>
              </TabsList>

              <TabsContent value="operations" className="space-y-3 pt-2">
                <div className="flex flex-col gap-2 md:flex-row md:items-center">
                  <div className="relative min-w-0 flex-1 md:max-w-sm">
                    <Search className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
                    <Input
                      aria-label={t("搜索缓存操作")}
                      className="pl-8"
                      value={search}
                      onChange={(event) => setSearch(event.target.value)}
                      placeholder={t("搜索站点、目标或操作 ID")}
                    />
                  </div>
                  <Select value={siteFilter} onValueChange={updateSiteFilter}>
                    <SelectTrigger
                      aria-label={t("筛选站点")}
                      className="w-full md:w-52"
                    >
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="all">{t("全部站点")}</SelectItem>
                      {query.data.sites.map((site) => (
                        <SelectItem key={site.site_id} value={site.site_id}>
                          {site.site_name}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <Select value={statusFilter} onValueChange={setStatusFilter}>
                    <SelectTrigger
                      aria-label={t("筛选状态")}
                      className="w-full md:w-40"
                    >
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="all">{t("全部状态")}</SelectItem>
                      <SelectItem value="applying">{t("应用中")}</SelectItem>
                      <SelectItem value="succeeded">{t("成功")}</SelectItem>
                      <SelectItem value="partial">{t("部分成功")}</SelectItem>
                      <SelectItem value="failed">{t("失败")}</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                {filteredOperations.length ? (
                  <Panel>
                    <div className="overflow-x-auto">
                      <Table className="min-w-[880px]">
                        <TableHeader>
                          <TableRow>
                            <TableHead>{t("操作")}</TableHead>
                            <TableHead>{t("站点")}</TableHead>
                            <TableHead>{t("范围 / 目标")}</TableHead>
                            <TableHead>{t("预热")}</TableHead>
                            <TableHead>{t("节点")}</TableHead>
                            <TableHead>{t("提交时间")}</TableHead>
                            <TableHead className="text-right">
                              {t("操作")}
                            </TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {operationPagination.items.map((operation) => (
                            <OperationRow
                              key={operation.id}
                              operation={operation}
                              retrying={
                                retryMutation.isPending &&
                                retryMutation.variables === operation.id
                              }
                              onInspect={() => setDetailID(operation.id)}
                              onRetry={() => retryMutation.mutate(operation.id)}
                            />
                          ))}
                        </TableBody>
                      </Table>
                    </div>
                    <ListPagination
                      pagination={operationPagination}
                      itemLabel={t("条操作")}
                    />
                  </Panel>
                ) : (
                  <EmptyState
                    title={t("没有匹配的缓存操作")}
                    description={t("调整筛选条件或创建新的失效操作")}
                  />
                )}
              </TabsContent>

              <TabsContent value="sites" className="pt-2">
                <SitesTable sites={query.data.sites} onCreate={openCreate} />
              </TabsContent>

              <TabsContent value="rules" className="pt-2">
                {query.data.rules.length ? (
                  <Panel>
                    <div className="overflow-x-auto">
                      <Table className="min-w-[680px]">
                        <TableHeader>
                          <TableRow>
                            <TableHead>{t("站点")}</TableHead>
                            <TableHead>{t("规则类型")}</TableHead>
                            <TableHead>{t("匹配目标")}</TableHead>
                            <TableHead>{t("规则代际")}</TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {query.data.rules.map((rule) => (
                            <TableRow
                              key={`${rule.site_id}-${rule.scope}-${rule.value}`}
                            >
                              <TableCell className="font-medium">
                                {rule.site_name}
                              </TableCell>
                              <TableCell>{scopeLabel(rule.scope)}</TableCell>
                              <TableCell>
                                <code className="text-xs">{rule.value}</code>
                              </TableCell>
                              <TableCell className="font-mono text-xs tabular-nums">
                                V{formatNumber(rule.generation)}
                              </TableCell>
                            </TableRow>
                          ))}
                        </TableBody>
                      </Table>
                    </div>
                  </Panel>
                ) : (
                  <EmptyState
                    title={t("当前没有 URL 或前缀规则")}
                    description={t("全站缓存代际仍由各站点独立维护")}
                  />
                )}
              </TabsContent>
            </Tabs>
          </>
        ) : null}
      </PageBody>

      <CreateOperationDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        sites={eligibleSites}
        selectedSite={selectedSite}
        value={draft}
        onChange={setDraft}
        pending={createMutation.isPending}
        onSubmit={() => createMutation.mutate(draft)}
      />
      <OperationDetailDialog
        operation={detailOperation}
        open={Boolean(detailID)}
        onOpenChange={(open) => {
          if (!open) setDetailID(null);
        }}
        retrying={retryMutation.isPending}
        onRetry={(operationID) => retryMutation.mutate(operationID)}
      />
    </>
  );
}

function OperationRow({
  operation,
  retrying,
  onInspect,
  onRetry,
}: {
  operation: CacheOperation;
  retrying: boolean;
  onInspect: () => void;
  onRetry: () => void;
}) {
  const targetNodes = operation.nodes.filter(
    (node) => node.configuration_status !== "not_targeted",
  );
  const succeededNodes = targetNodes.filter(
    (node) => node.configuration_status === "succeeded",
  ).length;
  const retryable =
    operation.prewarm_paths.length > 0 && !activeStatuses.has(operation.status);
  return (
    <TableRow>
      <TableCell>
        <div className="flex items-center gap-2">
          <StatusBadge status={operation.status} />
          <span className="text-xs text-muted-foreground">
            {operation.kind === "prewarm_retry" ? t("预热重试") : t("失效")}
          </span>
        </div>
        <code className="mt-1 block max-w-40 truncate text-[11px] text-muted-foreground">
          {operation.id}
        </code>
      </TableCell>
      <TableCell>
        <div
          className="max-w-44 truncate font-medium"
          title={operation.site_name}
        >
          {operation.site_name}
        </div>
        <div className="text-xs text-muted-foreground tabular-nums">
          Cache V{formatNumber(operation.cache_generation)}
        </div>
      </TableCell>
      <TableCell>
        <div className="text-xs">{scopeLabel(operation.scope)}</div>
        <code
          className="block max-w-64 truncate text-xs text-muted-foreground"
          title={operationTarget(operation)}
        >
          {operationTarget(operation)}
        </code>
      </TableCell>
      <TableCell>
        {operation.prewarm_paths.length ? (
          <div className="text-xs tabular-nums">
            {formatNumber(operation.prewarm_paths.length)} {t("个 URL")}
          </div>
        ) : (
          <span className="text-xs text-muted-foreground">{t("未启用")}</span>
        )}
      </TableCell>
      <TableCell className="text-xs tabular-nums">
        {targetNodes.length
          ? `${formatNumber(succeededNodes)} / ${formatNumber(targetNodes.length)}`
          : t("未下发")}
      </TableCell>
      <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
        {formatDateTime(operation.created_at)}
      </TableCell>
      <TableCell>
        <div className="flex justify-end gap-1">
          {retryable ? (
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  aria-label={t("重新预热")}
                  disabled={retrying}
                  onClick={onRetry}
                >
                  {retrying ? (
                    <LoaderCircle className="animate-spin" />
                  ) : (
                    <RotateCcw />
                  )}
                </Button>
              </TooltipTrigger>
              <TooltipContent>{t("重新预热")}</TooltipContent>
            </Tooltip>
          ) : null}
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                type="button"
                variant="ghost"
                size="icon-sm"
                aria-label={t("查看节点结果")}
                onClick={onInspect}
              >
                <Eye />
              </Button>
            </TooltipTrigger>
            <TooltipContent>{t("查看节点结果")}</TooltipContent>
          </Tooltip>
        </div>
      </TableCell>
    </TableRow>
  );
}

function SitesTable({
  sites,
  onCreate,
}: {
  sites: CacheSiteOverview[];
  onCreate: (siteID?: string) => void;
}) {
  return (
    <Panel>
      <div className="overflow-x-auto">
        <Table className="min-w-[820px]">
          <TableHeader>
            <TableRow>
              <TableHead>{t("站点")}</TableHead>
              <TableHead>{t("缓存状态")}</TableHead>
              <TableHead>{t("缓存代际")}</TableHead>
              <TableHead>{t("活动规则")}</TableHead>
              <TableHead>{t("节点")}</TableHead>
              <TableHead>{t("最近操作")}</TableHead>
              <TableHead className="text-right">{t("操作")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {sites.map((site) => (
              <TableRow key={site.site_id}>
                <TableCell>
                  <div
                    className="max-w-56 truncate font-medium"
                    title={site.site_name}
                  >
                    {site.site_name}
                  </div>
                  <div className="max-w-64 truncate text-xs text-muted-foreground">
                    {site.domains.join(", ") || site.site_id}
                  </div>
                </TableCell>
                <TableCell>
                  <StatusBadge
                    status={site.cacheable ? "succeeded" : "failed"}
                    label={
                      site.cacheable
                        ? site.pending_configuration
                          ? t("有待发布配置")
                          : t("可用")
                        : disabledReasonLabel(site.disabled_reason)
                    }
                  />
                </TableCell>
                <TableCell className="font-mono text-xs tabular-nums">
                  V{formatNumber(site.cache_generation)}
                </TableCell>
                <TableCell className="text-xs tabular-nums">
                  {formatNumber(site.rule_count)}
                </TableCell>
                <TableCell>
                  <div className="text-xs tabular-nums">
                    {formatNumber(site.active_node_count)} /{" "}
                    {formatNumber(site.node_count)} {t("在线")}
                  </div>
                  <div className="text-xs text-muted-foreground tabular-nums">
                    {formatNumber(site.reporting_node_count)} {t("个精确上报")}
                  </div>
                </TableCell>
                <TableCell>
                  {site.last_operation ? (
                    <div className="flex items-center gap-2">
                      <StatusBadge status={site.last_operation.status} />
                      <span className="whitespace-nowrap text-xs text-muted-foreground">
                        {formatDateTime(site.last_operation.created_at)}
                      </span>
                    </div>
                  ) : (
                    <span className="text-xs text-muted-foreground">
                      {t("暂无记录")}
                    </span>
                  )}
                </TableCell>
                <TableCell className="text-right">
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    disabled={!site.cacheable}
                    onClick={() => onCreate(site.site_id)}
                  >
                    <RefreshCw />
                    {t("失效 / 预热")}
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
    </Panel>
  );
}

function CreateOperationDialog({
  open,
  onOpenChange,
  sites,
  selectedSite,
  value,
  onChange,
  pending,
  onSubmit,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  sites: CacheSiteOverview[];
  selectedSite?: CacheSiteOverview;
  value: OperationDraft;
  onChange: (value: OperationDraft) => void;
  pending: boolean;
  onSubmit: () => void;
}) {
  const paths = splitPaths(value.prewarm_paths);
  const targetRequired = value.scope !== "full";
  const fullConfirmed =
    value.scope !== "full" ||
    (selectedSite && value.full_confirmation === selectedSite.site_name);
  const canSubmit =
    Boolean(selectedSite) &&
    (!targetRequired || Boolean(value.value.trim())) &&
    (!value.prewarm || value.scope !== "full" || paths.length > 0) &&
    Boolean(fullConfirmed);

  function submit(event: FormEvent) {
    event.preventDefault();
    if (canSubmit) onSubmit();
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100vh-2rem)] overflow-y-auto sm:max-w-2xl">
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{t("新建缓存操作")}</DialogTitle>
            <DialogDescription>
              {t("变更缓存键代际，并按需在目标边缘节点预热")}
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-5 py-4">
            <div className="grid gap-2">
              <Label htmlFor="cache-operation-site">{t("站点")}</Label>
              <Select
                value={value.site_id}
                onValueChange={(siteID) =>
                  onChange({
                    ...value,
                    site_id: siteID,
                    full_confirmation: "",
                  })
                }
              >
                <SelectTrigger id="cache-operation-site" className="w-full">
                  <SelectValue placeholder={t("选择站点")} />
                </SelectTrigger>
                <SelectContent>
                  {sites.map((site) => (
                    <SelectItem key={site.site_id} value={site.site_id}>
                      {site.site_name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>

            <div className="grid gap-2">
              <Label>{t("失效范围")}</Label>
              <Tabs
                value={value.scope}
                onValueChange={(scope) =>
                  onChange({
                    ...value,
                    scope: scope as CacheScope,
                    value: scope === "full" ? "" : value.value,
                    full_confirmation: "",
                  })
                }
              >
                <TabsList className="grid h-auto w-full grid-cols-3">
                  <TabsTrigger value="url">{t("单个 URL")}</TabsTrigger>
                  <TabsTrigger value="prefix">{t("路径前缀")}</TabsTrigger>
                  <TabsTrigger value="full">{t("整个站点")}</TabsTrigger>
                </TabsList>
              </Tabs>
            </div>

            {targetRequired ? (
              <div className="grid gap-2">
                <Label htmlFor="cache-operation-target">
                  {value.scope === "url" ? t("URL 路径") : t("路径前缀")}
                </Label>
                <Input
                  id="cache-operation-target"
                  value={value.value}
                  onChange={(event) =>
                    onChange({ ...value, value: event.target.value })
                  }
                  placeholder={
                    value.scope === "url" ? "/assets/app.js?v=2" : "/assets/"
                  }
                />
              </div>
            ) : null}

            <div className="flex items-center justify-between gap-4 border-y py-3">
              <div className="min-w-0">
                <Label htmlFor="cache-operation-prewarm">{t("同步预热")}</Label>
                <p className="mt-1 text-xs text-muted-foreground">
                  {t("配置生效后由每个目标边缘节点本机请求")}
                </p>
              </div>
              <Switch
                id="cache-operation-prewarm"
                checked={value.prewarm}
                onCheckedChange={(prewarm) => onChange({ ...value, prewarm })}
              />
            </div>

            {value.prewarm && value.scope !== "url" ? (
              <div className="grid gap-2">
                <Label htmlFor="cache-operation-paths">
                  {value.scope === "full"
                    ? t("预热 URL")
                    : t("补充预热 URL（可选）")}
                </Label>
                <Textarea
                  id="cache-operation-paths"
                  rows={5}
                  value={value.prewarm_paths}
                  onChange={(event) =>
                    onChange({ ...value, prewarm_paths: event.target.value })
                  }
                  placeholder={"/assets/app.js\n/assets/styles.css"}
                />
                <p className="text-xs text-muted-foreground tabular-nums">
                  {t("已选择 {value0} 个 URL", { value0: paths.length })}
                </p>
              </div>
            ) : null}

            {value.scope === "full" ? (
              <Alert variant="destructive">
                <AlertTriangle />
                <AlertTitle>{t("确认全站缓存失效")}</AlertTitle>
                <AlertDescription>
                  <p>
                    {t(
                      "此操作会推进全站缓存代际。旧缓存文件由正常淘汰策略回收，不会立即释放磁盘空间。",
                    )}
                  </p>
                  <div className="mt-3 grid gap-2">
                    <Label htmlFor="cache-operation-confirmation">
                      {t("输入站点名称 {value0} 继续", {
                        value0: selectedSite?.site_name ?? "",
                      })}
                    </Label>
                    <Input
                      id="cache-operation-confirmation"
                      autoComplete="off"
                      value={value.full_confirmation}
                      onChange={(event) =>
                        onChange({
                          ...value,
                          full_confirmation: event.target.value,
                        })
                      }
                    />
                  </div>
                </AlertDescription>
              </Alert>
            ) : (
              <Alert>
                <DatabaseZap />
                <AlertTitle>{t("操作预览")}</AlertTitle>
                <AlertDescription>
                  {scopeLabel(value.scope)} ·{" "}
                  {value.value.trim() || t("等待输入目标")}
                  {value.prewarm
                    ? ` · ${t("预热 {value0} 个显式 URL", {
                        value0: value.scope === "url" ? 1 : paths.length,
                      })}`
                    : ` · ${t("不预热")}`}
                </AlertDescription>
              </Alert>
            )}
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
            >
              {t("取消")}
            </Button>
            <Button
              type="submit"
              variant={value.scope === "full" ? "destructive" : "default"}
              disabled={pending || !canSubmit}
            >
              {pending ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <RefreshCw />
              )}
              {value.prewarm ? t("失效并预热") : t("执行失效")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function OperationDetailDialog({
  operation,
  open,
  onOpenChange,
  retrying,
  onRetry,
}: {
  operation?: CacheOperation;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  retrying: boolean;
  onRetry: (operationID: string) => void;
}) {
  if (!operation) return null;
  const retryable =
    operation.prewarm_paths.length > 0 && !activeStatuses.has(operation.status);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100vh-2rem)] overflow-y-auto sm:max-w-4xl">
        <DialogHeader>
          <DialogTitle>{operation.site_name}</DialogTitle>
          <DialogDescription>
            {operation.kind === "prewarm_retry" ? t("预热重试") : t("缓存失效")}{" "}
            · {formatDateTime(operation.created_at)}
          </DialogDescription>
        </DialogHeader>
        <div className="grid grid-cols-2 border-y sm:grid-cols-4">
          <DetailStat
            label={t("状态")}
            value={<StatusBadge status={operation.status} />}
          />
          <DetailStat label={t("范围")} value={scopeLabel(operation.scope)} />
          <DetailStat
            label={t("缓存代际")}
            value={`V${formatNumber(operation.cache_generation)}`}
          />
          <DetailStat
            label={t("预热 URL")}
            value={formatNumber(operation.prewarm_paths.length)}
          />
        </div>
        <div className="grid gap-1 text-sm">
          <span className="text-xs text-muted-foreground">{t("匹配目标")}</span>
          <code className="break-all text-xs">
            {operationTarget(operation)}
          </code>
          {operation.detail ? (
            <p className="mt-1 text-xs text-muted-foreground">
              {operation.detail}
            </p>
          ) : null}
        </div>
        {operation.prewarm_paths.length ? (
          <div className="grid gap-2">
            <h3 className="text-sm font-medium">{t("预热 URL")}</h3>
            <div className="max-h-32 overflow-y-auto border-y py-2">
              {operation.prewarm_paths.map((path) => (
                <code key={path} className="block break-all px-2 py-1 text-xs">
                  {path}
                </code>
              ))}
            </div>
          </div>
        ) : null}
        <div className="grid gap-2">
          <h3 className="text-sm font-medium">{t("逐节点结果")}</h3>
          {operation.nodes.length ? (
            <Panel>
              <div className="overflow-x-auto">
                <Table className="min-w-[840px] table-fixed">
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-36">{t("节点")}</TableHead>
                      <TableHead className="w-24">{t("配置下发")}</TableHead>
                      <TableHead className="w-24">{t("预热结果")}</TableHead>
                      <TableHead className="w-16">{t("URL")}</TableHead>
                      <TableHead className="w-72">{t("失败详情")}</TableHead>
                      <TableHead className="w-40">{t("上报时间")}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {operation.nodes.map((node) => (
                      <TableRow key={node.node_id}>
                        <TableCell className="whitespace-normal">
                          <div className="break-all font-medium">
                            {node.node_name}
                          </div>
                          {node.target_version ? (
                            <div className="font-mono text-[11px] text-muted-foreground">
                              State V{formatNumber(node.target_version)}
                            </div>
                          ) : null}
                        </TableCell>
                        <TableCell>
                          <StatusBadge status={node.configuration_status} />
                        </TableCell>
                        <TableCell>
                          <StatusBadge status={node.warmup_status} />
                        </TableCell>
                        <TableCell className="text-xs tabular-nums">
                          {node.attempted_urls
                            ? `${formatNumber(node.succeeded_urls)} / ${formatNumber(node.attempted_urls)}`
                            : "--"}
                        </TableCell>
                        <TableCell className="whitespace-normal text-xs">
                          {node.failures.length ? (
                            <div className="space-y-1">
                              {node.failures.map((failure, index) => (
                                <div
                                  key={`${failure.path ?? "job"}-${index}`}
                                  className="break-words"
                                >
                                  {failure.path ? (
                                    <code className="mr-1 break-all">
                                      {failure.path}
                                    </code>
                                  ) : null}
                                  <span className="text-destructive">
                                    {failure.detail}
                                  </span>
                                </div>
                              ))}
                            </div>
                          ) : (
                            <span className="text-muted-foreground">--</span>
                          )}
                        </TableCell>
                        <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                          {node.reported_at
                            ? formatDateTime(node.reported_at)
                            : "--"}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
            </Panel>
          ) : (
            <EmptyState
              title={t("本次操作没有目标节点")}
              description={t("配置已暂存，节点恢复或分配后需重新发布")}
            />
          )}
        </div>
        <DialogFooter>
          {retryable ? (
            <Button
              type="button"
              variant="outline"
              disabled={retrying}
              onClick={() => onRetry(operation.id)}
            >
              {retrying ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <RotateCcw />
              )}
              {t("重新预热")}
            </Button>
          ) : null}
          <Button type="button" onClick={() => onOpenChange(false)}>
            {t("完成")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function OverviewStat({
  icon: Icon,
  label,
  value,
}: {
  icon: typeof Activity;
  label: string;
  value: string;
}) {
  return (
    <div className="flex min-w-0 items-center gap-3 border-r px-3 py-3 last:border-r-0 sm:px-4">
      <Icon className="size-4 shrink-0 text-muted-foreground" />
      <div className="min-w-0">
        <div className="truncate text-xs text-muted-foreground">{label}</div>
        <div className="font-mono text-base font-semibold tabular-nums">
          {value}
        </div>
      </div>
    </div>
  );
}

function DetailStat({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="min-w-0 border-r px-3 py-3 last:border-r-0">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="mt-1 text-sm font-medium">{value}</div>
    </div>
  );
}

function emptyDraft(siteID: string): OperationDraft {
  return {
    site_id: siteID,
    scope: "url",
    value: "",
    prewarm: false,
    prewarm_paths: "",
    full_confirmation: "",
  };
}

function splitPaths(value: string) {
  return [
    ...new Set(
      value
        .split(/[\n,]/)
        .map((item) => item.trim())
        .filter(Boolean),
    ),
  ];
}

function scopeLabel(scope: string) {
  if (scope === "url") return t("单个 URL");
  if (scope === "prefix") return t("路径前缀");
  return t("整个站点");
}

function operationTarget(operation: Pick<CacheOperation, "scope" | "target">) {
  return operation.scope === "full" ? t("整个站点") : operation.target || "--";
}

function disabledReasonLabel(reason?: CacheSiteOverview["disabled_reason"]) {
  const labels: Record<string, string> = {
    deleting: t("正在删除"),
    tcp_only: t("纯 TCP 站点"),
    passthrough: t("透传模式"),
    origin_response_buffering_disabled: t("回源响应缓冲已关闭"),
    unsupported_origin: t("源站协议不支持缓存"),
  };
  return labels[reason ?? ""] ?? t("缓存不可用");
}
