import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  CalendarClock,
  Gauge,
  LoaderCircle,
  Pencil,
  Plus,
  RefreshCw,
  Route,
  ShieldCheck,
  Trash2,
} from "lucide-react";
import { useState, type ReactNode } from "react";
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
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { useListPagination } from "@/hooks/use-list-pagination";
import { api, errorMessage, jsonBody } from "@/lib/api";
import { formatDateTime, formatNumber } from "@/lib/format";
import { t, useI18n } from "@/lib/i18n";
import { toneSurface } from "@/lib/tones";
import type {
  SmartRoutingConfig,
  SmartRoutingNode,
  SmartRoutingOverview,
  SmartRoutingWindow,
} from "@/lib/types";
import { cn } from "@/lib/utils";

export function SchedulingPage() {
  useI18n();
  const queryClient = useQueryClient();
  const [editingNode, setEditingNode] = useState<SmartRoutingNode | null>(null);
  const query = useQuery({
    queryKey: ["monitoring", "smart-routing"],
    queryFn: () => api<SmartRoutingOverview>("/api/monitoring/smart-routing"),
    refetchInterval: 10_000,
  });
  const pagination = useListPagination(query.data?.nodes ?? []);
  const update = useMutation({
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
      toast.success(t("智能路由设置已更新"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const nodes = query.data?.nodes ?? [];
  const configured = nodes.filter(
    (node) => node.score.enabled || node.schedule.enabled,
  );
  const managed = nodes.filter((node) => node.enabled);
  const blocked = managed.filter((node) => node.blocked_by.length > 0);
  const scheduled = nodes.filter((node) => node.schedule.enabled);

  return (
    <>
      <PageHeader
        title={t("调度")}
        description={t("智能路由策略与边缘流量门控")}
        actions={
          <Button
            variant="outline"
            size="icon"
            aria-label={t("刷新调度数据")}
            disabled={query.isFetching}
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
          <>
            <section className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
              <Summary
                icon={<Route />}
                label={t("已配置节点")}
                value={`${configured.length} / ${nodes.length}`}
              />
              <Summary
                icon={<ShieldCheck />}
                label={t("已接管节点")}
                value={formatNumber(managed.length)}
              />
              <Summary
                icon={<Gauge />}
                label={t("受阻节点")}
                value={formatNumber(blocked.length)}
                danger={blocked.length > 0}
              />
              <Summary
                icon={<CalendarClock />}
                label={t("时间规则")}
                value={formatNumber(scheduled.length)}
              />
            </section>

            {nodes.length ? (
              <Panel>
                <Table className="min-w-[1120px]">
                  <TableHeader>
                    <TableRow>
                      <TableHead className="pl-5">{t("节点")}</TableHead>
                      <TableHead>{t("智能路由")}</TableHead>
                      <TableHead>{t("调度")}</TableHead>
                      <TableHead>{t("评分门控")}</TableHead>
                      <TableHead>{t("当前评分")}</TableHead>
                      <TableHead>{t("时间门控")}</TableHead>
                      <TableHead>{t("阻断原因")}</TableHead>
                      <TableHead>{t("下次切换")}</TableHead>
                      <TableHead className="w-14 pr-5 text-right">
                        {t("操作")}
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {pagination.items.map((node) => (
                      <SmartRoutingRow
                        key={node.node_id}
                        node={node}
                        busy={update.isPending}
                        onToggle={(enabled) =>
                          update.mutate({
                            node,
                            config: smartRoutingConfigFromNode(node, enabled),
                          })
                        }
                        onEdit={() => setEditingNode(node)}
                      />
                    ))}
                  </TableBody>
                </Table>
                <ListPagination
                  pagination={pagination}
                  itemLabel={t("个节点")}
                />
              </Panel>
            ) : (
              <EmptyState
                title={t("暂无边缘节点")}
                description={t("节点尚未配置智能路由规则")}
              />
            )}
          </>
        ) : null}
      </PageBody>
      <SmartRoutingDialog
        key={editingNode?.node_id ?? "closed"}
        node={editingNode}
        timezone={query.data?.timezone ?? "Asia/Shanghai"}
        busy={update.isPending}
        onOpenChange={(open) => {
          if (!open) setEditingNode(null);
        }}
        onSave={(config) => {
          if (!editingNode) return;
          update.mutate(
            { node: editingNode, config },
            { onSuccess: () => setEditingNode(null) },
          );
        }}
      />
    </>
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
      ? { status: "failed", label: t("已阻断") }
      : node.score.gate === "allowed"
        ? { status: "succeeded", label: t("已放行") }
        : { status: "pending", label: t("待判定") };
  const scheduleState =
    node.schedule.gate === "open"
      ? { status: "succeeded", label: t("窗口内") }
      : { status: "failed", label: t("窗口外") };
  const blockedBy = node.blocked_by
    .map((reason) => (reason === "score" ? "评分" : "时间"))
    .join("、");
  const blockedByLabel = t(blockedBy);

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
            aria-label={t(
              node.enabled ? "停用 {name} 智能路由" : "启用 {name} 智能路由",
              { name: node.name },
            )}
            onCheckedChange={onToggle}
          />
          <span className="text-xs text-muted-foreground">
            {t(node.enabled ? "启用" : "人工接管")}
          </span>
        </div>
      </TableCell>
      <TableCell>
        <StatusBadge
          status={node.status}
          label={node.auto_paused ? t("智能暂停") : undefined}
        />
      </TableCell>
      <TableCell>
        {node.score.enabled ? (
          <div className="space-y-1">
            <StatusBadge status={scoreState.status} label={scoreState.label} />
            <div className="whitespace-nowrap text-xs text-muted-foreground">
              &lt; {node.score.pause_below_score} x{" "}
              {node.score.pause_after_rounds}
              <span className="mx-1">/</span>&gt;= {node.score.resume_at_score}{" "}
              x {node.score.resume_after_rounds}
            </div>
          </div>
        ) : (
          <span className="text-muted-foreground">{t("未启用")}</span>
        )}
      </TableCell>
      <TableCell>
        {!node.capable ? (
          <span className="text-muted-foreground">{t("不支持拨测")}</span>
        ) : node.score.current_score === undefined ? (
          <span className="text-muted-foreground">{t("等待上报")}</span>
        ) : (
          <div>
            <span className="font-medium tabular-nums">
              {node.score.current_score}
            </span>
            {node.score.stale ? (
              <div className="text-xs text-muted-foreground">
                {t("数据过期")}
              </div>
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
              {t("{count} 个窗口", {
                count: node.schedule.windows.length,
              })}
            </div>
          </div>
        ) : (
          <span className="text-muted-foreground">{t("未启用")}</span>
        )}
      </TableCell>
      <TableCell>
        {node.enabled ? (
          blockedByLabel ? (
            <StatusBadge status="failed" label={blockedByLabel} />
          ) : (
            <StatusBadge status="succeeded" label={t("无")} />
          )
        ) : (
          <StatusBadge status="pending" label={t("人工接管")} />
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
              aria-label={t("编辑 {name} 智能路由", { name: node.name })}
              onClick={onEdit}
            >
              <Pencil />
            </Button>
          </TooltipTrigger>
          <TooltipContent>{t("编辑智能路由")}</TooltipContent>
        </Tooltip>
      </TableCell>
    </TableRow>
  );
}

const weekdays = [
  { value: 1, label: "周一" },
  { value: 2, label: "周二" },
  { value: 3, label: "周三" },
  { value: 4, label: "周四" },
  { value: 5, label: "周五" },
  { value: 6, label: "周六" },
  { value: 7, label: "周日" },
] as const;

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
            <DialogTitle>{t("智能路由")}</DialogTitle>
            <DialogDescription>
              {node?.name} · {node?.public_ipv4}
            </DialogDescription>
          </DialogHeader>

          <div className="grid gap-5 py-5">
            <div className="flex items-center justify-between gap-4">
              <div>
                <Label htmlFor="smart-routing-enabled">{t("自动调度")}</Label>
                <div className="mt-1 text-xs text-muted-foreground">
                  {t(config.enabled ? "智能路由接管" : "人工接管")}
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
                  <h3 className="text-sm font-medium">{t("评分门控")}</h3>
                  <div className="mt-1 text-xs text-muted-foreground">
                    {t("当前评分")} {node?.score.current_score ?? "--"}
                  </div>
                </div>
                <Switch
                  checked={config.score.enabled}
                  aria-label={t("启用评分门控")}
                  onCheckedChange={(enabled) => updateScore({ enabled })}
                />
              </div>
              <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
                <NumberField
                  id="smart-pause-score"
                  label={t("暂停分数")}
                  value={config.score.pause_below_score}
                  disabled={!config.score.enabled}
                  min={1}
                  max={100}
                  onChange={(value) =>
                    updateScore({ pause_below_score: value })
                  }
                />
                <NumberField
                  id="smart-pause-rounds"
                  label={t("暂停轮数")}
                  value={config.score.pause_after_rounds}
                  disabled={!config.score.enabled}
                  min={1}
                  max={120}
                  onChange={(value) =>
                    updateScore({ pause_after_rounds: value })
                  }
                />
                <NumberField
                  id="smart-resume-score"
                  label={t("恢复分数")}
                  value={config.score.resume_at_score}
                  disabled={!config.score.enabled}
                  min={1}
                  max={100}
                  onChange={(value) => updateScore({ resume_at_score: value })}
                />
                <NumberField
                  id="smart-resume-rounds"
                  label={t("恢复轮数")}
                  value={config.score.resume_after_rounds}
                  disabled={!config.score.enabled}
                  min={minimumSmartRoutingResumeRounds}
                  max={120}
                  onChange={(value) =>
                    updateScore({ resume_after_rounds: value })
                  }
                />
              </div>
            </section>

            <Separator />

            <section className="grid gap-4">
              <div className="flex items-center justify-between gap-4">
                <div>
                  <h3 className="text-sm font-medium">{t("时间门控")}</h3>
                  <div className="mt-1 text-xs text-muted-foreground">
                    {timezone}
                  </div>
                </div>
                <Switch
                  checked={config.schedule.enabled}
                  aria-label={t("启用时间门控")}
                  onCheckedChange={(enabled) => updateSchedule({ enabled })}
                />
              </div>

              {config.schedule.windows.map((window, index) => (
                <div key={index} className="grid gap-3 rounded-md border p-3">
                  <div className="flex items-center justify-between gap-3">
                    <span className="text-xs font-medium">
                      {t("时间窗 {index}", { index: index + 1 })}
                    </span>
                    <Tooltip>
                      <TooltipTrigger asChild>
                        <Button
                          type="button"
                          variant="ghost"
                          size="icon-sm"
                          aria-label={t("删除时间窗 {index}", {
                            index: index + 1,
                          })}
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
                      <TooltipContent>{t("删除时间窗")}</TooltipContent>
                    </Tooltip>
                  </div>
                  <div
                    className="flex flex-wrap gap-x-4 gap-y-2"
                    role="group"
                    aria-label={t("时间窗 {index} 星期", { index: index + 1 })}
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
                        {t(weekday.label)}
                      </label>
                    ))}
                  </div>
                  <div className="grid grid-cols-2 gap-3">
                    <div className="grid gap-2">
                      <Label htmlFor={`smart-window-${index}-start`}>
                        {t("开始")}
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
                      <Label htmlFor={`smart-window-${index}-end`}>
                        {t("结束")}
                      </Label>
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
                {t("添加时间窗")}
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
              {t("取消")}
            </Button>
            <Button type="submit" disabled={busy || Boolean(validationError)}>
              {busy ? <LoaderCircle className="animate-spin" /> : <Route />}
              {t("保存")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function NumberField({
  id,
  label,
  value,
  disabled,
  min,
  max,
  onChange,
}: {
  id: string;
  label: string;
  value: number;
  disabled: boolean;
  min: number;
  max: number;
  onChange: (value: number) => void;
}) {
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        type="number"
        min={min}
        max={max}
        disabled={disabled}
        value={value}
        onChange={(event) => onChange(Number(event.target.value))}
      />
    </div>
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
    return t("至少启用一个门控规则");
  }
  if (
    config.score.pause_below_score < 1 ||
    config.score.pause_below_score > 100 ||
    config.score.resume_at_score < 1 ||
    config.score.resume_at_score > 100
  ) {
    return t("评分阈值必须在 1 到 100 之间");
  }
  if (config.score.resume_at_score < config.score.pause_below_score) {
    return t("恢复分数不能低于暂停分数");
  }
  if (
    config.score.pause_after_rounds < 1 ||
    config.score.pause_after_rounds > 120
  ) {
    return t("暂停轮数必须在 1 到 120 之间");
  }
  if (
    config.score.resume_after_rounds < minimumSmartRoutingResumeRounds ||
    config.score.resume_after_rounds > 120
  ) {
    return t("恢复轮数必须在 {min} 到 120 之间", {
      min: minimumSmartRoutingResumeRounds,
    });
  }
  if (config.schedule.enabled && config.schedule.windows.length === 0) {
    return t("时间门控至少需要一个时间窗");
  }
  if (
    config.schedule.windows.some(
      (window) => window.weekdays.length === 0 || !window.start || !window.end,
    )
  ) {
    return t("每个时间窗都需要星期、开始和结束时间");
  }
  return "";
}

function Summary({
  icon,
  label,
  value,
  danger = false,
}: {
  icon: ReactNode;
  label: string;
  value: string;
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
        </div>
      </CardContent>
    </Card>
  );
}
