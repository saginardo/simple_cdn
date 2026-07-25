import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Activity,
  ChevronRight,
  CirclePlus,
  Clock,
  Gauge,
  LoaderCircle,
  Pencil,
  RefreshCw,
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
import { usePersistentEnum } from "@/hooks/use-persistent-state";
import { api, errorMessage, jsonBody } from "@/lib/api";
import { formatDateTime, formatNumber } from "@/lib/format";
import { toneSurface } from "@/lib/tones";
import type {
  MonitoringNode,
  MonitoringOverview,
  MonitoringTarget,
} from "@/lib/types";
import { cn } from "@/lib/utils";
import { t, useI18n } from "@/lib/i18n";
export function MonitoringPage() {
  useI18n();
  const queryClient = useQueryClient();
  const [section, setSection] = usePersistentEnum(
    "simple-cdn.monitoring.tab",
    ["nodes", "results", "targets"] as const,
    "nodes",
  );
  const [createOpen, setCreateOpen] = useState(false);
  const [removeTarget, setRemoveTarget] = useState<MonitoringTarget | null>(
    null,
  );
  const [editTarget, setEditTarget] = useState<MonitoringTarget | null>(null);
  const query = useQuery({
    queryKey: ["monitoring"],
    queryFn: () => api<MonitoringOverview>("/api/monitoring"),
    refetchInterval: 10_000,
  });
  const refresh = () => {
    void queryClient.invalidateQueries({
      queryKey: ["monitoring"],
    });
  };
  const toggleTarget = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      api<MonitoringTarget>(
        `/api/monitoring/targets/${encodeURIComponent(id)}`,
        {
          method: "PUT",
          ...jsonBody({
            enabled,
          }),
        },
      ),
    onSuccess: (target) => {
      void refresh();
      toast.success(target.enabled ? t("拨测目标已启用") : t("拨测目标已停用"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const remove = useMutation({
    mutationFn: (target: MonitoringTarget) =>
      api<{
        ok: boolean;
      }>(`/api/monitoring/targets/${encodeURIComponent(target.id)}`, {
        method: "DELETE",
      }),
    onSuccess: () => {
      setRemoveTarget(null);
      void refresh();
      toast.success(t("拨测目标已删除"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const data = query.data;
  const nodesPagination = useListPagination(data?.nodes ?? []);
  const results = useMemo(
    () =>
      (data?.nodes ?? []).flatMap((node) =>
        node.results.map((result) => ({
          node,
          result,
        })),
      ),
    [data?.nodes],
  );
  const resultsPagination = useListPagination(results);
  const targetsPagination = useListPagination(data?.targets ?? []);
  const enabledTargets = data?.targets.filter((target) => target.enabled) ?? [];
  const capableNodes = data?.nodes.filter((node) => node.capable) ?? [];
  const healthyNodes = capableNodes.filter(
    (node) =>
      !node.stale &&
      node.score !== undefined &&
      node.score >= (data?.healthy_score ?? 80),
  );
  const unhealthyNodes = capableNodes.filter(
    (node) =>
      node.stale ||
      (node.score !== undefined && node.score < (data?.healthy_score ?? 80)),
  );
  return (
    <>
      <PageHeader
        title={t("监测")}
        description={t("边缘 TCP 可达性、访问时延与节点评分")}
        actions={
          <>
            <Button
              variant="outline"
              size="icon"
              aria-label={t("刷新监测数据")}
              onClick={() => void query.refetch()}
            >
              <RefreshCw className={query.isFetching ? "animate-spin" : ""} />
            </Button>
            <Button onClick={() => setCreateOpen(true)}>
              <CirclePlus />
              {t("添加目标")}
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
                label={t("启用目标")}
                value={formatNumber(enabledTargets.length)}
                detail={t("{value0} 秒 / 每轮 {value1} 次", {
                  value0: data.interval_seconds,
                  value1: data.attempts_per_round,
                })}
              />
              <Summary
                icon={<Server />}
                label={t("监测覆盖")}
                value={`${capableNodes.length} / ${data.nodes.length}`}
                detail={t("已支持节点")}
              />
              <Summary
                icon={<Gauge />}
                label={t("当前正常")}
                value={`${healthyNodes.length} / ${capableNodes.length}`}
                detail={t("健康线 {value0} 分", {
                  value0: data.healthy_score,
                })}
              />
              <Summary
                icon={<Clock />}
                label={t("需关注节点")}
                value={formatNumber(unhealthyNodes.length)}
                detail={t("评分偏低或数据过期")}
                danger={Boolean(unhealthyNodes.length)}
              />
            </section>

            {!enabledTargets.length ? (
              <EmptyState
                title={t("暂无启用的拨测目标")}
                description={t(
                  "添加或启用目标后，边缘节点会开始上报 TCP 拨测结果",
                )}
              />
            ) : null}

            <Tabs
              value={section}
              onValueChange={(value) => setSection(value as typeof section)}
              className="space-y-4"
            >
              <TabsList>
                <TabsTrigger value="nodes">{t("节点评分")}</TabsTrigger>
                <TabsTrigger value="results">{t("拨测明细")}</TabsTrigger>
                <TabsTrigger value="targets">{t("目标配置")}</TabsTrigger>
              </TabsList>
              <TabsContent value="nodes">
                {data.nodes.length ? (
                  <Panel>
                    <Table className="min-w-[940px]">
                      <TableHeader>
                        <TableRow>
                          <TableHead className="pl-5">{t("节点")}</TableHead>
                          <TableHead>{t("调度")}</TableHead>
                          <TableHead>{t("监测")}</TableHead>
                          <TableHead className="w-44">{t("评分")}</TableHead>
                          <TableHead>{t("成功率")}</TableHead>
                          <TableHead>{t("平均时延")}</TableHead>
                          <TableHead>{t("连续异常")}</TableHead>
                          <TableHead className="pr-5">
                            {t("最后拨测")}
                          </TableHead>
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
                      itemLabel={t("个节点")}
                    />
                  </Panel>
                ) : (
                  <EmptyState title={t("暂无边缘节点")} />
                )}
              </TabsContent>
              <TabsContent value="results">
                {results.length ? (
                  <Panel>
                    <Table className="min-w-[820px]">
                      <TableHeader>
                        <TableRow>
                          <TableHead className="pl-5">{t("节点")}</TableHead>
                          <TableHead>{t("拨测目标")}</TableHead>
                          <TableHead>{t("TCP 结果")}</TableHead>
                          <TableHead>{t("成功次数")}</TableHead>
                          <TableHead>{t("平均时延")}</TableHead>
                          <TableHead className="pr-5">
                            {t("拨测时间")}
                          </TableHead>
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
                                  label={succeeded ? t("可达") : t("异常")}
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
                      itemLabel={t("条结果")}
                    />
                  </Panel>
                ) : (
                  <EmptyState title={t("等待节点上报拨测结果")} />
                )}
              </TabsContent>
              <TabsContent value="targets">
                {data.targets.length ? (
                  <Panel>
                    <Table className="min-w-[660px]">
                      <TableHeader>
                        <TableRow>
                          <TableHead className="pl-5">{t("名称")}</TableHead>
                          <TableHead>{t("目标地址")}</TableHead>
                          <TableHead>{t("状态")}</TableHead>
                          <TableHead>{t("更新时间")}</TableHead>
                          <TableHead className="w-24 pr-5 text-right">
                            {t("操作")}
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
                                  aria-label={`${target.enabled ? t("停用") : t("启用")} ${target.name}`}
                                  onCheckedChange={(enabled) =>
                                    toggleTarget.mutate({
                                      id: target.id,
                                      enabled,
                                    })
                                  }
                                />
                                <span className="text-xs text-muted-foreground">
                                  {target.enabled ? t("启用") : t("停用")}
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
                                      aria-label={t("重命名 {value0}", {
                                        value0: target.name,
                                      })}
                                      onClick={() => setEditTarget(target)}
                                    >
                                      <Pencil />
                                    </Button>
                                  </TooltipTrigger>
                                  <TooltipContent>
                                    {t("重命名目标")}
                                  </TooltipContent>
                                </Tooltip>
                                <Tooltip>
                                  <TooltipTrigger asChild>
                                    <Button
                                      variant="ghost"
                                      size="icon-sm"
                                      aria-label={t("删除 {value0}", {
                                        value0: target.name,
                                      })}
                                      onClick={() => setRemoveTarget(target)}
                                    >
                                      <Trash2 />
                                    </Button>
                                  </TooltipTrigger>
                                  <TooltipContent>
                                    {t("删除目标")}
                                  </TooltipContent>
                                </Tooltip>
                              </div>
                            </TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                    <ListPagination
                      pagination={targetsPagination}
                      itemLabel={t("个目标")}
                    />
                  </Panel>
                ) : (
                  <EmptyState title={t("暂无拨测目标")} />
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
      <ConfirmDialog
        open={Boolean(removeTarget)}
        onOpenChange={(open) => {
          if (!open) setRemoveTarget(null);
        }}
        title={t("删除拨测目标")}
        description={t("将删除 {value0}（{value1}）的当前拨测结果。", {
          value0: removeTarget?.name ?? t("该目标"),
          value1: removeTarget?.address ?? t("未知地址"),
        })}
        confirmLabel={t("删除")}
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
    ? {
        status: "pending",
        label: t("待升级"),
      }
    : node.score === undefined
      ? {
          status: "pending",
          label: t("等待上报"),
        }
      : node.stale
        ? {
            status: "pending",
            label: t("数据过期"),
          }
        : node.score >= healthyScore
          ? {
              status: "succeeded",
              label: t("正常"),
            }
          : {
              status: "failed",
              label: t("异常"),
            };
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
          label={node.monitor_auto_paused ? t("智能暂停") : undefined}
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
            aria-label={t("查看 {value0} 拨测历史", {
              value0: node.name,
            })}
            onClick={(event) => event.stopPropagation()}
          >
            <ChevronRight />
          </Link>
        </Button>
      </TableCell>
    </TableRow>
  );
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
        ...jsonBody({
          name,
          address,
        }),
      }),
    onSuccess: () => {
      setName("");
      setAddress("");
      onOpenChange(false);
      void queryClient.invalidateQueries({
        queryKey: ["monitoring"],
      });
      toast.success(t("拨测目标已添加"));
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
            <DialogTitle>{t("添加拨测目标")}</DialogTitle>
            <DialogDescription>{t("配置 TCP 连接目标")}</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-5">
            <div className="grid gap-2">
              <Label htmlFor="monitoring-name">{t("名称")}</Label>
              <Input
                id="monitoring-name"
                value={name}
                onChange={(event) => setName(event.target.value)}
                placeholder={t("主 API")}
                maxLength={64}
                autoComplete="off"
                autoFocus
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="monitoring-address">
                {t("IP:端口 或 域名:端口")}
              </Label>
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
              {t("取消")}
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
              {t("添加")}
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
        {
          method: "PUT",
          ...jsonBody({
            name,
          }),
        },
      ),
    onSuccess: () => {
      onOpenChange(false);
      void queryClient.invalidateQueries({
        queryKey: ["monitoring"],
      });
      toast.success(t("拨测目标名称已更新"));
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
            <DialogTitle>{t("重命名拨测目标")}</DialogTitle>
            <DialogDescription>{target?.address}</DialogDescription>
          </DialogHeader>
          <div className="grid gap-2 py-5">
            <Label htmlFor="monitoring-edit-name">{t("名称")}</Label>
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
              {t("取消")}
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
              {t("保存")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
