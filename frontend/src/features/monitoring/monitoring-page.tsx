import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Activity,
  ChevronRight,
  CirclePlus,
  Clock,
  Gauge,
  LoaderCircle,
  Pencil,
  Plus,
  RefreshCw,
  Route,
  Server,
  Trash2,
} from "lucide-react";
import { useMemo, useState, type FormEvent, type ReactNode } from "react";
import { Link, useNavigate } from "react-router-dom";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/confirm-dialog";
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
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Checkbox } from "@/components/ui/checkbox";
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
import { Progress } from "@/components/ui/progress";
import { Separator } from "@/components/ui/separator";
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
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { useListPagination } from "@/hooks/use-list-pagination";
import { api, errorMessage, jsonBody } from "@/lib/api";
import { formatDateTime, formatNumber } from "@/lib/format";
import { toneSurface } from "@/lib/tones";
import type {
  MonitoringNode,
  MonitoringOverview,
  MonitoringTarget,
  SmartRoutingConfig,
  SmartRoutingNode,
  SmartRoutingOverview,
  SmartRoutingWindow,
} from "@/lib/types";
import { cn } from "@/lib/utils";

export function MonitoringPage() {
  const queryClient = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  const [removeTarget, setRemoveTarget] = useState<MonitoringTarget | null>(
    null,
  );
  const [editTarget, setEditTarget] = useState<MonitoringTarget | null>(null);
  const [editSmartRouting, setEditSmartRouting] =
    useState<SmartRoutingNode | null>(null);
  const query = useQuery({
    queryKey: ["monitoring"],
    queryFn: () => api<MonitoringOverview>("/api/monitoring"),
    refetchInterval: 10_000,
  });
  const smartRoutingQuery = useQuery({
    queryKey: ["monitoring", "smart-routing"],
    queryFn: () => api<SmartRoutingOverview>("/api/monitoring/smart-routing"),
    refetchInterval: 10_000,
  });
  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["monitoring"] });
  };
  const toggleTarget = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      api<MonitoringTarget>(
        `/api/monitoring/targets/${encodeURIComponent(id)}`,
        { method: "PUT", ...jsonBody({ enabled }) },
      ),
    onSuccess: (target) => {
      void refresh();
      toast.success(target.enabled ? "拨测目标已启用" : "拨测目标已停用");
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const remove = useMutation({
    mutationFn: (target: MonitoringTarget) =>
      api<{ ok: boolean }>(
        `/api/monitoring/targets/${encodeURIComponent(target.id)}`,
        { method: "DELETE" },
      ),
    onSuccess: () => {
      setRemoveTarget(null);
      void refresh();
      toast.success("拨测目标已删除");
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const data = query.data;
  const nodesPagination = useListPagination(data?.nodes ?? []);
  const results = useMemo(
    () =>
      (data?.nodes ?? []).flatMap((node) =>
        node.results.map((result) => ({ node, result })),
      ),
    [data?.nodes],
  );
  const resultsPagination = useListPagination(results);
  const targetsPagination = useListPagination(data?.targets ?? []);
  const smartRoutingPagination = useListPagination(
    smartRoutingQuery.data?.nodes ?? [],
  );
  const updateSmartRouting = useMutation({
    mutationFn: ({
      node,
      config,
    }: {
      node: SmartRoutingNode;
      config: SmartRoutingConfig;
    }) =>
      api<{ ok: boolean }>(
        `/api/monitoring/nodes/${encodeURIComponent(node.node_id)}/smart-routing`,
        { method: "PUT", ...jsonBody(config) },
      ),
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["monitoring", "smart-routing"],
      });
      void queryClient.invalidateQueries({ queryKey: ["monitoring"] });
      toast.success("智能路由设置已更新");
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const enabledTargets = data?.targets.filter((target) => target.enabled) ?? [];
  const capableNodes = data?.nodes.filter((node) => node.capable) ?? [];
  const healthyNodes = capableNodes.filter(
    (node) =>
      !node.stale &&
      node.score !== undefined &&
      node.score >= (data?.healthy_score ?? 80),
  );
  const autoPaused = data?.nodes.filter(
    (node) => node.monitor_auto_paused,
  ).length;

  return (
    <>
      <PageHeader
        title="监测"
        description="边缘 TCP 可达性、访问时延与调度状态"
        actions={
          <>
            <Button
              variant="outline"
              size="icon"
              aria-label="刷新监测数据"
              onClick={() => {
                void query.refetch();
                void smartRoutingQuery.refetch();
              }}
            >
              <RefreshCw className={query.isFetching ? "animate-spin" : ""} />
            </Button>
            <Button onClick={() => setCreateOpen(true)}>
              <CirclePlus />
              添加目标
            </Button>
          </>
        }
      />
      <PageBody>
        {query.isLoading ? <PageLoading /> : null}
        {query.error ? <PageError error={query.error} /> : null}
        {data ? (
          <>
            <section className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
              <Summary
                icon={<Activity />}
                label="启用目标"
                value={formatNumber(enabledTargets.length)}
                detail={`${data.interval_seconds} 秒 / 每轮 ${data.attempts_per_round} 次`}
              />
              <Summary
                icon={<Server />}
                label="监测覆盖"
                value={`${capableNodes.length} / ${data.nodes.length}`}
                detail="已支持节点"
              />
              <Summary
                icon={<Gauge />}
                label="当前正常"
                value={`${healthyNodes.length} / ${capableNodes.length}`}
                detail={`健康线 ${data.healthy_score} 分`}
              />
              <Summary
                icon={<Clock />}
                label="智能路由暂停"
                value={formatNumber(autoPaused)}
                detail="评分或时间规则"
                danger={Boolean(autoPaused)}
              />
            </section>

            {!enabledTargets.length ? (
              <EmptyState
                title="暂无启用的拨测目标"
                description="添加或启用目标后，边缘节点会开始上报 TCP 拨测结果"
              />
            ) : null}

            <Tabs defaultValue="nodes" className="space-y-4">
              <TabsList>
                <TabsTrigger value="nodes">节点评分</TabsTrigger>
                <TabsTrigger value="smart-routing">智能路由</TabsTrigger>
                <TabsTrigger value="results">拨测明细</TabsTrigger>
                <TabsTrigger value="targets">目标配置</TabsTrigger>
              </TabsList>
              <TabsContent value="nodes">
                {data.nodes.length ? (
                  <Panel>
                    <Table className="min-w-[940px]">
                      <TableHeader>
                        <TableRow>
                          <TableHead className="pl-5">节点</TableHead>
                          <TableHead>调度</TableHead>
                          <TableHead>监测</TableHead>
                          <TableHead className="w-44">评分</TableHead>
                          <TableHead>成功率</TableHead>
                          <TableHead>平均时延</TableHead>
                          <TableHead>连续异常</TableHead>
                          <TableHead className="pr-5">最后拨测</TableHead>
                          <TableHead className="w-10" />
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {nodesPagination.items.map((node) => (
                          <NodeRow
                            key={node.node_id}
                            node={node}
                            healthyScore={data.healthy_score}
                          />
                        ))}
                      </TableBody>
                    </Table>
                    <ListPagination
                      pagination={nodesPagination}
                      itemLabel="个节点"
                    />
                  </Panel>
                ) : (
                  <EmptyState title="暂无边缘节点" />
                )}
              </TabsContent>
              <TabsContent value="smart-routing">
                {smartRoutingQuery.isLoading ? <PageLoading /> : null}
                {smartRoutingQuery.error ? (
                  <PageError error={smartRoutingQuery.error} />
                ) : null}
                {smartRoutingQuery.data ? (
                  smartRoutingQuery.data.nodes.length ? (
                    <Panel>
                      <Table className="min-w-[1120px]">
                        <TableHeader>
                          <TableRow>
                            <TableHead className="pl-5">节点</TableHead>
                            <TableHead>智能路由</TableHead>
                            <TableHead>调度</TableHead>
                            <TableHead>评分门控</TableHead>
                            <TableHead>当前评分</TableHead>
                            <TableHead>时间门控</TableHead>
                            <TableHead>阻断原因</TableHead>
                            <TableHead>下次切换</TableHead>
                            <TableHead className="w-14 pr-5 text-right">
                              操作
                            </TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          {smartRoutingPagination.items.map((node) => (
                            <SmartRoutingRow
                              key={node.node_id}
                              node={node}
                              busy={updateSmartRouting.isPending}
                              onToggle={(enabled) =>
                                updateSmartRouting.mutate({
                                  node,
                                  config: smartRoutingConfigFromNode(
                                    node,
                                    enabled,
                                  ),
                                })
                              }
                              onEdit={() => setEditSmartRouting(node)}
                            />
                          ))}
                        </TableBody>
                      </Table>
                      <ListPagination
                        pagination={smartRoutingPagination}
                        itemLabel="个节点"
                      />
                    </Panel>
                  ) : (
                    <EmptyState title="暂无边缘节点" />
                  )
                ) : null}
              </TabsContent>
              <TabsContent value="results">
                {results.length ? (
                  <Panel>
                    <Table className="min-w-[820px]">
                      <TableHeader>
                        <TableRow>
                          <TableHead className="pl-5">节点</TableHead>
                          <TableHead>拨测目标</TableHead>
                          <TableHead>TCP 结果</TableHead>
                          <TableHead>成功次数</TableHead>
                          <TableHead>平均时延</TableHead>
                          <TableHead className="pr-5">拨测时间</TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {resultsPagination.items.map(({ node, result }) => {
                          const succeeded =
                            result.successful_attempts === result.attempts;
                          return (
                            <TableRow
                              key={`${node.node_id}:${result.target_id}`}
                            >
                              <TableCell className="pl-5">
                                <div className="font-medium">{node.name}</div>
                                <div className="font-mono text-xs text-muted-foreground">
                                  {node.public_ipv4}
                                </div>
                              </TableCell>
                              <TableCell>
                                <div className="font-medium">
                                  {result.target_name}
                                </div>
                                <div className="font-mono text-xs text-muted-foreground">
                                  {result.address}
                                </div>
                              </TableCell>
                              <TableCell>
                                <StatusBadge
                                  status={succeeded ? "succeeded" : "failed"}
                                  label={succeeded ? "可达" : "异常"}
                                />
                                {result.error ? (
                                  <div
                                    className="mt-1 max-w-64 truncate text-xs text-muted-foreground"
                                    title={result.error}
                                  >
                                    {result.error}
                                  </div>
                                ) : null}
                              </TableCell>
                              <TableCell className="tabular-nums">
                                {result.successful_attempts} / {result.attempts}
                              </TableCell>
                              <TableCell className="tabular-nums">
                                {result.successful_attempts
                                  ? `${result.average_latency_ms.toFixed(1)} ms`
                                  : "--"}
                              </TableCell>
                              <TableCell className="pr-5 whitespace-nowrap text-xs text-muted-foreground">
                                {formatDateTime(result.checked_at)}
                              </TableCell>
                            </TableRow>
                          );
                        })}
                      </TableBody>
                    </Table>
                    <ListPagination
                      pagination={resultsPagination}
                      itemLabel="条结果"
                    />
                  </Panel>
                ) : (
                  <EmptyState title="等待节点上报拨测结果" />
                )}
              </TabsContent>
              <TabsContent value="targets">
                {data.targets.length ? (
                  <Panel>
                    <Table className="min-w-[660px]">
                      <TableHeader>
                        <TableRow>
                          <TableHead className="pl-5">名称</TableHead>
                          <TableHead>目标地址</TableHead>
                          <TableHead>状态</TableHead>
                          <TableHead>更新时间</TableHead>
                          <TableHead className="w-24 pr-5 text-right">
                            操作
                          </TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {targetsPagination.items.map((target) => (
                          <TableRow key={target.id}>
                            <TableCell className="pl-5 font-medium">
                              {target.name}
                            </TableCell>
                            <TableCell className="font-mono text-xs">
                              {target.address}
                            </TableCell>
                            <TableCell>
                              <div className="flex items-center gap-2">
                                <Switch
                                  checked={target.enabled}
                                  disabled={toggleTarget.isPending}
                                  aria-label={`${target.enabled ? "停用" : "启用"} ${target.name}`}
                                  onCheckedChange={(enabled) =>
                                    toggleTarget.mutate({
                                      id: target.id,
                                      enabled,
                                    })
                                  }
                                />
                                <span className="text-xs text-muted-foreground">
                                  {target.enabled ? "启用" : "停用"}
                                </span>
                              </div>
                            </TableCell>
                            <TableCell className="text-xs text-muted-foreground">
                              {formatDateTime(target.updated_at)}
                            </TableCell>
                            <TableCell className="pr-5 text-right">
                              <div className="flex justify-end gap-1">
                                <Tooltip>
                                  <TooltipTrigger asChild>
                                    <Button
                                      variant="ghost"
                                      size="icon-sm"
                                      aria-label={`重命名 ${target.name}`}
                                      onClick={() => setEditTarget(target)}
                                    >
                                      <Pencil />
                                    </Button>
                                  </TooltipTrigger>
                                  <TooltipContent>重命名目标</TooltipContent>
                                </Tooltip>
                                <Tooltip>
                                  <TooltipTrigger asChild>
                                    <Button
                                      variant="ghost"
                                      size="icon-sm"
                                      aria-label={`删除 ${target.name}`}
                                      onClick={() => setRemoveTarget(target)}
                                    >
                                      <Trash2 />
                                    </Button>
                                  </TooltipTrigger>
                                  <TooltipContent>删除目标</TooltipContent>
                                </Tooltip>
                              </div>
                            </TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                    <ListPagination
                      pagination={targetsPagination}
                      itemLabel="个目标"
                    />
                  </Panel>
                ) : (
                  <EmptyState title="暂无拨测目标" />
                )}
              </TabsContent>
            </Tabs>
          </>
        ) : null}
      </PageBody>
      <CreateTargetDialog open={createOpen} onOpenChange={setCreateOpen} />
      <EditTargetDialog
        key={editTarget?.id ?? "closed"}
        target={editTarget}
        onOpenChange={(open) => {
          if (!open) setEditTarget(null);
        }}
      />
      <SmartRoutingDialog
        key={editSmartRouting?.node_id ?? "closed"}
        node={editSmartRouting}
        timezone={smartRoutingQuery.data?.timezone ?? "Asia/Shanghai"}
        busy={updateSmartRouting.isPending}
        onOpenChange={(open) => {
          if (!open) setEditSmartRouting(null);
        }}
        onSave={(config) => {
          if (!editSmartRouting) return;
          updateSmartRouting.mutate(
            { node: editSmartRouting, config },
            { onSuccess: () => setEditSmartRouting(null) },
          );
        }}
      />
      <ConfirmDialog
        open={Boolean(removeTarget)}
        onOpenChange={(open) => {
          if (!open) setRemoveTarget(null);
        }}
        title="删除拨测目标"
        description={`将删除 ${removeTarget?.name ?? "该目标"}（${removeTarget?.address ?? "未知地址"}）的当前拨测结果。`}
        confirmLabel="删除"
        destructive
        busy={remove.isPending}
        onConfirm={() => {
          if (removeTarget) remove.mutate(removeTarget);
        }}
      />
    </>
  );
}

function NodeRow({
  node,
  healthyScore,
}: {
  node: MonitoringNode;
  healthyScore: number;
}) {
  const navigate = useNavigate();
  const monitoringState = !node.capable
    ? { status: "pending", label: "待升级" }
    : node.score === undefined
      ? { status: "pending", label: "等待上报" }
      : node.stale
        ? { status: "pending", label: "数据过期" }
        : node.score >= healthyScore
          ? { status: "succeeded", label: "正常" }
          : { status: "failed", label: "异常" };
  const historyPath = `/monitoring/nodes/${encodeURIComponent(node.node_id)}`;
  return (
    <TableRow className="cursor-pointer" onClick={() => navigate(historyPath)}>
      <TableCell className="pl-5">
        <div className="font-medium">{node.name}</div>
        <div className="font-mono text-xs text-muted-foreground">
          {node.public_ipv4}
        </div>
      </TableCell>
      <TableCell>
        <StatusBadge
          status={node.status}
          label={node.monitor_auto_paused ? "智能暂停" : undefined}
        />
      </TableCell>
      <TableCell>
        <StatusBadge
          status={monitoringState.status}
          label={monitoringState.label}
        />
      </TableCell>
      <TableCell>
        {node.score === undefined ? (
          <span className="text-muted-foreground">--</span>
        ) : (
          <div className="grid w-36 grid-cols-[2rem_1fr] items-center gap-2">
            <span className="font-medium tabular-nums">{node.score}</span>
            <Progress value={node.score} />
          </div>
        )}
      </TableCell>
      <TableCell className="tabular-nums">
        {node.success_rate === undefined
          ? "--"
          : `${node.success_rate.toFixed(1)}%`}
      </TableCell>
      <TableCell className="tabular-nums">
        {node.average_latency_ms === undefined
          ? "--"
          : `${node.average_latency_ms.toFixed(1)} ms`}
      </TableCell>
      <TableCell className="tabular-nums">
        {formatNumber(node.consecutive_abnormal)}
      </TableCell>
      <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
        {formatDateTime(node.last_checked_at)}
      </TableCell>
      <TableCell className="pr-5">
        <Button asChild variant="ghost" size="icon-sm">
          <Link
            to={historyPath}
            aria-label={`查看 ${node.name} 拨测历史`}
            onClick={(event) => event.stopPropagation()}
          >
            <ChevronRight />
          </Link>
        </Button>
      </TableCell>
    </TableRow>
  );
}

function SmartRoutingRow({
  node,
  busy,
  onToggle,
  onEdit,
}: {
  node: SmartRoutingNode;
  busy: boolean;
  onToggle: (enabled: boolean) => void;
  onEdit: () => void;
}) {
  const canEnable = node.score.enabled || node.schedule.enabled;
  const scoreState =
    node.score.gate === "blocked"
      ? { status: "failed", label: "已阻断" }
      : node.score.gate === "allowed"
        ? { status: "succeeded", label: "已放行" }
        : { status: "pending", label: "待判定" };
  const scheduleState =
    node.schedule.gate === "open"
      ? { status: "succeeded", label: "窗口内" }
      : { status: "failed", label: "窗口外" };
  const blockedBy = node.blocked_by
    .map((reason) => (reason === "score" ? "评分" : "时间"))
    .join("、");

  return (
    <TableRow>
      <TableCell className="pl-5">
        <div className="font-medium">{node.name}</div>
        <div className="font-mono text-xs text-muted-foreground">
          {node.public_ipv4}
        </div>
      </TableCell>
      <TableCell>
        <div className="flex items-center gap-2">
          <Switch
            checked={node.enabled}
            disabled={busy || (!node.enabled && !canEnable)}
            aria-label={`${node.enabled ? "停用" : "启用"} ${node.name} 智能路由`}
            onCheckedChange={onToggle}
          />
          <span className="text-xs text-muted-foreground">
            {node.enabled ? "启用" : "人工接管"}
          </span>
        </div>
      </TableCell>
      <TableCell>
        <StatusBadge
          status={node.status}
          label={node.auto_paused ? "智能暂停" : undefined}
        />
      </TableCell>
      <TableCell>
        {node.score.enabled ? (
          <div className="space-y-1">
            <StatusBadge status={scoreState.status} label={scoreState.label} />
            <div className="whitespace-nowrap text-xs text-muted-foreground">
              &lt; {node.score.pause_below_score} ×{" "}
              {node.score.pause_after_rounds}
              <span className="mx-1">/</span>≥ {node.score.resume_at_score} ×{" "}
              {node.score.resume_after_rounds}
            </div>
          </div>
        ) : (
          <span className="text-muted-foreground">未启用</span>
        )}
      </TableCell>
      <TableCell>
        {!node.capable ? (
          <span className="text-muted-foreground">不支持拨测</span>
        ) : node.score.current_score === undefined ? (
          <span className="text-muted-foreground">等待上报</span>
        ) : (
          <div>
            <span className="font-medium tabular-nums">
              {node.score.current_score}
            </span>
            {node.score.stale ? (
              <div className="text-xs text-muted-foreground">数据过期</div>
            ) : null}
          </div>
        )}
      </TableCell>
      <TableCell>
        {node.schedule.enabled ? (
          <div className="space-y-1">
            <StatusBadge
              status={scheduleState.status}
              label={scheduleState.label}
            />
            <div className="text-xs text-muted-foreground">
              {node.schedule.windows.length} 个窗口
            </div>
          </div>
        ) : (
          <span className="text-muted-foreground">未启用</span>
        )}
      </TableCell>
      <TableCell>
        {node.enabled ? (
          blockedBy ? (
            <StatusBadge status="failed" label={blockedBy} />
          ) : (
            <StatusBadge status="succeeded" label="无" />
          )
        ) : (
          <StatusBadge status="pending" label="人工接管" />
        )}
      </TableCell>
      <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
        {node.schedule.enabled
          ? formatDateTime(node.schedule.next_transition_at)
          : "--"}
      </TableCell>
      <TableCell className="pr-5 text-right">
        <Tooltip>
          <TooltipTrigger asChild>
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label={`编辑 ${node.name} 智能路由`}
              onClick={onEdit}
            >
              <Pencil />
            </Button>
          </TooltipTrigger>
          <TooltipContent>编辑智能路由</TooltipContent>
        </Tooltip>
      </TableCell>
    </TableRow>
  );
}

const weekdays = [
  { value: 1, label: "一" },
  { value: 2, label: "二" },
  { value: 3, label: "三" },
  { value: 4, label: "四" },
  { value: 5, label: "五" },
  { value: 6, label: "六" },
  { value: 7, label: "日" },
];

const minimumSmartRoutingResumeRounds = 3;

function SmartRoutingDialog({
  node,
  timezone,
  busy,
  onOpenChange,
  onSave,
}: {
  node: SmartRoutingNode | null;
  timezone: string;
  busy: boolean;
  onOpenChange: (open: boolean) => void;
  onSave: (config: SmartRoutingConfig) => void;
}) {
  const [config, setConfig] = useState<SmartRoutingConfig>(() =>
    node ? smartRoutingConfigFromNode(node) : emptySmartRoutingConfig(),
  );
  const validationError = smartRoutingValidationError(config);

  function updateScore(values: Partial<SmartRoutingConfig["score"]>) {
    setConfig((current) => ({
      ...current,
      score: { ...current.score, ...values },
    }));
  }

  function updateSchedule(values: Partial<SmartRoutingConfig["schedule"]>) {
    setConfig((current) => ({
      ...current,
      schedule: { ...current.schedule, ...values },
    }));
  }

  function updateWindow(index: number, window: SmartRoutingWindow) {
    updateSchedule({
      windows: config.schedule.windows.map((current, currentIndex) =>
        currentIndex === index ? window : current,
      ),
    });
  }

  return (
    <Dialog open={Boolean(node)} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-3xl">
        <form
          onSubmit={(event) => {
            event.preventDefault();
            if (!validationError) onSave(config);
          }}
        >
          <DialogHeader>
            <DialogTitle>智能路由</DialogTitle>
            <DialogDescription>
              {node?.name} · {node?.public_ipv4}
            </DialogDescription>
          </DialogHeader>

          <div className="grid gap-5 py-5">
            <div className="flex items-center justify-between gap-4">
              <div>
                <Label htmlFor="smart-routing-enabled">自动调度</Label>
                <div className="mt-1 text-xs text-muted-foreground">
                  {config.enabled ? "智能路由接管" : "人工接管"}
                </div>
              </div>
              <Switch
                id="smart-routing-enabled"
                checked={config.enabled}
                onCheckedChange={(enabled) =>
                  setConfig((current) => ({ ...current, enabled }))
                }
              />
            </div>

            <Separator />

            <section className="grid gap-4">
              <div className="flex items-center justify-between gap-4">
                <div>
                  <h3 className="text-sm font-medium">评分门控</h3>
                  <div className="mt-1 text-xs text-muted-foreground">
                    当前评分 {node?.score.current_score ?? "--"}
                  </div>
                </div>
                <Switch
                  checked={config.score.enabled}
                  aria-label="启用评分门控"
                  onCheckedChange={(enabled) => updateScore({ enabled })}
                />
              </div>
              <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
                <div className="grid gap-2">
                  <Label htmlFor="smart-pause-score">暂停分数</Label>
                  <Input
                    id="smart-pause-score"
                    type="number"
                    min={1}
                    max={100}
                    disabled={!config.score.enabled}
                    value={config.score.pause_below_score}
                    onChange={(event) =>
                      updateScore({
                        pause_below_score: Number(event.target.value),
                      })
                    }
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="smart-pause-rounds">暂停轮数</Label>
                  <Input
                    id="smart-pause-rounds"
                    type="number"
                    min={1}
                    max={120}
                    disabled={!config.score.enabled}
                    value={config.score.pause_after_rounds}
                    onChange={(event) =>
                      updateScore({
                        pause_after_rounds: Number(event.target.value),
                      })
                    }
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="smart-resume-score">恢复分数</Label>
                  <Input
                    id="smart-resume-score"
                    type="number"
                    min={1}
                    max={100}
                    disabled={!config.score.enabled}
                    value={config.score.resume_at_score}
                    onChange={(event) =>
                      updateScore({
                        resume_at_score: Number(event.target.value),
                      })
                    }
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="smart-resume-rounds">恢复轮数</Label>
                  <Input
                    id="smart-resume-rounds"
                    type="number"
                    min={minimumSmartRoutingResumeRounds}
                    max={120}
                    disabled={!config.score.enabled}
                    value={config.score.resume_after_rounds}
                    onChange={(event) =>
                      updateScore({
                        resume_after_rounds: Number(event.target.value),
                      })
                    }
                  />
                </div>
              </div>
            </section>

            <Separator />

            <section className="grid gap-4">
              <div className="flex items-center justify-between gap-4">
                <div>
                  <h3 className="text-sm font-medium">时间门控</h3>
                  <div className="mt-1 text-xs text-muted-foreground">
                    {timezone}
                  </div>
                </div>
                <Switch
                  checked={config.schedule.enabled}
                  aria-label="启用时间门控"
                  onCheckedChange={(enabled) => updateSchedule({ enabled })}
                />
              </div>

              {config.schedule.windows.map((window, index) => (
                <div key={index} className="grid gap-3 rounded-md border p-3">
                  <div className="flex items-center justify-between gap-3">
                    <span className="text-xs font-medium">
                      时间窗 {index + 1}
                    </span>
                    <Tooltip>
                      <TooltipTrigger asChild>
                        <Button
                          type="button"
                          variant="ghost"
                          size="icon-sm"
                          aria-label={`删除时间窗 ${index + 1}`}
                          onClick={() =>
                            updateSchedule({
                              windows: config.schedule.windows.filter(
                                (_, currentIndex) => currentIndex !== index,
                              ),
                            })
                          }
                        >
                          <Trash2 />
                        </Button>
                      </TooltipTrigger>
                      <TooltipContent>删除时间窗</TooltipContent>
                    </Tooltip>
                  </div>
                  <div
                    className="flex flex-wrap gap-x-4 gap-y-2"
                    role="group"
                    aria-label={`时间窗 ${index + 1} 星期`}
                  >
                    {weekdays.map((weekday) => (
                      <label
                        key={weekday.value}
                        className="flex cursor-pointer items-center gap-2 text-sm"
                      >
                        <Checkbox
                          checked={window.weekdays.includes(weekday.value)}
                          onCheckedChange={(checked) => {
                            const selected = Boolean(checked);
                            const values = selected
                              ? [...window.weekdays, weekday.value]
                              : window.weekdays.filter(
                                  (value) => value !== weekday.value,
                                );
                            updateWindow(index, {
                              ...window,
                              weekdays: [...new Set(values)].sort(
                                (left, right) => left - right,
                              ),
                            });
                          }}
                        />
                        周{weekday.label}
                      </label>
                    ))}
                  </div>
                  <div className="grid grid-cols-2 gap-3">
                    <div className="grid gap-2">
                      <Label htmlFor={`smart-window-${index}-start`}>
                        开始
                      </Label>
                      <Input
                        id={`smart-window-${index}-start`}
                        type="time"
                        value={window.start}
                        onChange={(event) =>
                          updateWindow(index, {
                            ...window,
                            start: event.target.value,
                          })
                        }
                      />
                    </div>
                    <div className="grid gap-2">
                      <Label htmlFor={`smart-window-${index}-end`}>结束</Label>
                      <Input
                        id={`smart-window-${index}-end`}
                        type="time"
                        value={window.end}
                        onChange={(event) =>
                          updateWindow(index, {
                            ...window,
                            end: event.target.value,
                          })
                        }
                      />
                    </div>
                  </div>
                </div>
              ))}

              <Button
                type="button"
                variant="outline"
                className="w-fit"
                disabled={config.schedule.windows.length >= 32}
                onClick={() =>
                  updateSchedule({
                    windows: [
                      ...config.schedule.windows,
                      {
                        weekdays: [1, 2, 3, 4, 5],
                        start: "09:00",
                        end: "18:00",
                      },
                    ],
                  })
                }
              >
                <Plus />
                添加时间窗
              </Button>
            </section>

            {validationError ? (
              <div className="text-sm text-destructive">{validationError}</div>
            ) : null}
          </div>

          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              disabled={busy}
              onClick={() => onOpenChange(false)}
            >
              取消
            </Button>
            <Button type="submit" disabled={busy || Boolean(validationError)}>
              {busy ? <LoaderCircle className="animate-spin" /> : <Route />}
              保存
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function smartRoutingConfigFromNode(
  node: SmartRoutingNode,
  enabled = node.enabled,
): SmartRoutingConfig {
  return {
    enabled,
    score: {
      enabled: node.score.enabled,
      pause_below_score: node.score.pause_below_score,
      pause_after_rounds: node.score.pause_after_rounds,
      resume_at_score: node.score.resume_at_score,
      resume_after_rounds: node.score.resume_after_rounds,
    },
    schedule: {
      enabled: node.schedule.enabled,
      windows: node.schedule.windows.map((window) => ({
        ...window,
        weekdays: [...window.weekdays],
      })),
    },
  };
}

function emptySmartRoutingConfig(): SmartRoutingConfig {
  return {
    enabled: true,
    score: {
      enabled: true,
      pause_below_score: 80,
      pause_after_rounds: 4,
      resume_at_score: 80,
      resume_after_rounds: minimumSmartRoutingResumeRounds,
    },
    schedule: { enabled: false, windows: [] },
  };
}

function smartRoutingValidationError(config: SmartRoutingConfig) {
  if (config.enabled && !config.score.enabled && !config.schedule.enabled) {
    return "至少启用一个门控规则";
  }
  if (
    config.score.pause_below_score < 1 ||
    config.score.pause_below_score > 100 ||
    config.score.resume_at_score < 1 ||
    config.score.resume_at_score > 100
  ) {
    return "评分阈值必须在 1 到 100 之间";
  }
  if (config.score.resume_at_score < config.score.pause_below_score) {
    return "恢复分数不能低于暂停分数";
  }
  if (
    config.score.pause_after_rounds < 1 ||
    config.score.pause_after_rounds > 120
  ) {
    return "暂停轮数必须在 1 到 120 之间";
  }
  if (
    config.score.resume_after_rounds < minimumSmartRoutingResumeRounds ||
    config.score.resume_after_rounds > 120
  ) {
    return `恢复轮数必须在 ${minimumSmartRoutingResumeRounds} 到 120 之间`;
  }
  if (config.schedule.enabled && config.schedule.windows.length === 0) {
    return "时间门控至少需要一个时间窗";
  }
  if (
    config.schedule.windows.some(
      (window) => window.weekdays.length === 0 || !window.start || !window.end,
    )
  ) {
    return "每个时间窗都需要星期、开始和结束时间";
  }
  return "";
}

function Summary({
  icon,
  label,
  value,
  detail,
  danger = false,
}: {
  icon: ReactNode;
  label: string;
  value: string;
  detail: string;
  danger?: boolean;
}) {
  return (
    <Card size="sm">
      <CardContent className="flex items-center gap-3">
        <div
          className={cn(
            "grid size-9 shrink-0 place-items-center rounded-md [&_svg]:size-4",
            toneSurface[danger ? "danger" : "neutral"],
          )}
        >
          {icon}
        </div>
        <div className="min-w-0">
          <div className="text-xs text-muted-foreground">{label}</div>
          <div className="text-lg font-semibold tabular-nums">{value}</div>
          <div className="truncate text-xs text-muted-foreground">{detail}</div>
        </div>
      </CardContent>
    </Card>
  );
}

function CreateTargetDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [address, setAddress] = useState("");
  const mutation = useMutation({
    mutationFn: () =>
      api<MonitoringTarget>("/api/monitoring/targets", {
        method: "POST",
        ...jsonBody({ name, address }),
      }),
    onSuccess: () => {
      setName("");
      setAddress("");
      onOpenChange(false);
      void queryClient.invalidateQueries({ queryKey: ["monitoring"] });
      toast.success("拨测目标已添加");
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    mutation.mutate();
  }
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>添加拨测目标</DialogTitle>
            <DialogDescription>配置 TCP 连接目标</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-5">
            <div className="grid gap-2">
              <Label htmlFor="monitoring-name">名称</Label>
              <Input
                id="monitoring-name"
                value={name}
                onChange={(event) => setName(event.target.value)}
                placeholder="主 API"
                maxLength={64}
                autoComplete="off"
                autoFocus
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="monitoring-address">IP:端口 或 域名:端口</Label>
              <Input
                id="monitoring-address"
                value={address}
                onChange={(event) => setAddress(event.target.value)}
                placeholder="probe.example.com:443"
                autoComplete="off"
                spellCheck={false}
              />
            </div>
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              disabled={mutation.isPending}
              onClick={() => onOpenChange(false)}
            >
              取消
            </Button>
            <Button
              type="submit"
              disabled={!name.trim() || !address.trim() || mutation.isPending}
            >
              {mutation.isPending ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <CirclePlus />
              )}
              添加
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function EditTargetDialog({
  target,
  onOpenChange,
}: {
  target: MonitoringTarget | null;
  onOpenChange: (open: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const [name, setName] = useState(target?.name ?? "");
  const mutation = useMutation({
    mutationFn: () =>
      api<MonitoringTarget>(
        `/api/monitoring/targets/${encodeURIComponent(target?.id ?? "")}`,
        { method: "PUT", ...jsonBody({ name }) },
      ),
    onSuccess: () => {
      onOpenChange(false);
      void queryClient.invalidateQueries({ queryKey: ["monitoring"] });
      toast.success("拨测目标名称已更新");
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    mutation.mutate();
  }
  return (
    <Dialog open={Boolean(target)} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>重命名拨测目标</DialogTitle>
            <DialogDescription>{target?.address}</DialogDescription>
          </DialogHeader>
          <div className="grid gap-2 py-5">
            <Label htmlFor="monitoring-edit-name">名称</Label>
            <Input
              id="monitoring-edit-name"
              value={name}
              onChange={(event) => setName(event.target.value)}
              maxLength={64}
              autoComplete="off"
              autoFocus
            />
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              disabled={mutation.isPending}
              onClick={() => onOpenChange(false)}
            >
              取消
            </Button>
            <Button
              type="submit"
              disabled={
                !target ||
                !name.trim() ||
                name.trim() === target.name ||
                mutation.isPending
              }
            >
              {mutation.isPending ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <Pencil />
              )}
              保存
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
