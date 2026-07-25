import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Ban,
  CirclePlus,
  LoaderCircle,
  LockOpen,
  Pencil,
  Rocket,
  ShieldCheck,
  Trash2,
  Zap,
} from "lucide-react";
import { useState, type FormEvent } from "react";
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
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
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
import { api, errorMessage } from "@/lib/api";
import { useListPagination } from "@/hooks/use-list-pagination";
import { usePersistentEnum } from "@/hooks/use-persistent-state";
import { formatDateTime, formatNumber } from "@/lib/format";
import type {
  RateLimitPolicy,
  SecurityOverview,
  SecurityPolicy,
} from "@/lib/types";
import { t, useI18n } from "@/lib/i18n";
export function SecurityPage() {
  useI18n();
  const queryClient = useQueryClient();
  const [section, setSection] = usePersistentEnum(
    "simple-cdn.security.tab",
    ["policies", "rate", "bans", "events", "nodes"] as const,
    "policies",
  );
  const [policy, setPolicy] = useState<SecurityPolicy | "new" | null>(null);
  const [rateLimit, setRateLimit] = useState<RateLimitPolicy | "new" | null>(
    null,
  );
  const [remove, setRemove] = useState<{
    kind: "policy" | "rate";
    id: string;
    name: string;
  } | null>(null);
  const query = useQuery({
    queryKey: ["security"],
    queryFn: () => api<SecurityOverview>("/api/security"),
    refetchInterval: 15_000,
  });
  const applyResult = (data: SecurityOverview) =>
    queryClient.setQueryData(["security"], data);
  const deploy = useMutation({
    mutationFn: () =>
      api<SecurityOverview>("/api/security/deploy", {
        method: "POST",
      }),
    onSuccess: (data) => {
      applyResult(data);
      toast.success(t("安全策略已重新发布"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const removeMutation = useMutation({
    mutationFn: (target: NonNullable<typeof remove>) =>
      api<SecurityOverview>(
        `/api/security/${target.kind === "policy" ? "policies" : "rate-limit-policies"}/${encodeURIComponent(target.id)}`,
        {
          method: "DELETE",
        },
      ),
    onSuccess: (data) => {
      applyResult(data);
      setRemove(null);
      toast.success(t("策略已删除并发布"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const unban = useMutation({
    mutationFn: (ip: string) =>
      api<SecurityOverview>(`/api/security/bans/${encodeURIComponent(ip)}`, {
        method: "DELETE",
      }),
    onSuccess: (data) => {
      applyResult(data);
      toast.success(t("IP 封禁已解除"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const data = query.data;
  const policiesPagination = useListPagination(data?.policies ?? []);
  const rateLimitPagination = useListPagination(
    data?.rate_limit_policies ?? [],
  );
  const bansPagination = useListPagination(data?.bans ?? []);
  const eventsPagination = useListPagination(data?.events ?? []);
  const nodesPagination = useListPagination(data?.nodes ?? []);
  const enabled = data
    ? data.policies.filter((item) => item.enabled).length +
      data.rate_limit_policies.filter((item) => item.enabled).length
    : 0;
  const eligibleNodes =
    data?.nodes.filter((node) =>
      ["active", "draining"].includes(node.status),
    ) ?? [];
  const capableNodes = eligibleNodes.filter(
    (node) => node.capable && node.rate_limit_capable,
  );
  const appliedNodes = capableNodes.filter(
    (node) =>
      node.configured &&
      node.rate_limit_configured &&
      node.desired_version > 0 &&
      node.applied_version >= node.desired_version,
  );
  return (
    <>
      <PageHeader
        title={t("安全")}
        description={t("边缘访问策略、请求限速与活动封禁")}
        actions={
          <>
            <Button
              variant="outline"
              disabled={deploy.isPending}
              onClick={() => deploy.mutate()}
            >
              {deploy.isPending ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <Rocket />
              )}
              {t("重新发布")}
            </Button>
            <Button onClick={() => setPolicy("new")}>
              <CirclePlus />
              {t("访问策略")}
            </Button>
          </>
        }
      />
      <PageBody>
        {query.isLoading ? <PageLoading /> : null}
        {query.error ? <PageError error={query.error} /> : null}
        {data ? (
          <>
            {data.deployment_error ? (
              <Alert variant="destructive">
                <AlertTitle>{t("部分策略未能发布")}</AlertTitle>
                <AlertDescription>{data.deployment_error}</AlertDescription>
              </Alert>
            ) : null}
            <section className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
              <Summary
                icon={ShieldCheck}
                label={t("启用策略")}
                value={formatNumber(enabled)}
              />
              <Summary
                icon={Ban}
                label={t("活动封禁")}
                value={formatNumber(data.active_ban_count)}
              />
              <Summary
                icon={Zap}
                label={t("能力覆盖")}
                value={`${capableNodes.length} / ${eligibleNodes.length}`}
              />
              <Summary
                icon={Rocket}
                label={t("已应用节点")}
                value={`${appliedNodes.length} / ${capableNodes.length}`}
              />
            </section>
            <Tabs
              value={section}
              onValueChange={(value) => setSection(value as typeof section)}
              className="space-y-4"
            >
              <TabsList>
                <TabsTrigger value="policies">{t("访问策略")}</TabsTrigger>
                <TabsTrigger value="rate">{t("请求限速")}</TabsTrigger>
                <TabsTrigger value="bans">{t("活动封禁")}</TabsTrigger>
                <TabsTrigger value="events">{t("最近命中")}</TabsTrigger>
                <TabsTrigger value="nodes">{t("节点覆盖")}</TabsTrigger>
              </TabsList>
              <TabsContent value="policies">
                <SectionHeader
                  title={t("通用访问策略")}
                  meta={t("按优先级匹配规范化路径")}
                  action={
                    <Button size="sm" onClick={() => setPolicy("new")}>
                      <CirclePlus />
                      {t("新增")}
                    </Button>
                  }
                />
                <DataFrame
                  empty={!data.policies.length}
                  emptyTitle={t("暂无访问策略")}
                  footer={
                    <ListPagination
                      pagination={policiesPagination}
                      itemLabel={t("个策略")}
                    />
                  }
                >
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>{t("策略")}</TableHead>
                        <TableHead>{t("表达式")}</TableHead>
                        <TableHead>{t("动作")}</TableHead>
                        <TableHead>{t("优先级")}</TableHead>
                        <TableHead>{t("状态")}</TableHead>
                        <TableHead className="text-right">
                          {t("操作")}
                        </TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {policiesPagination.items.map((item) => (
                        <TableRow key={item.id}>
                          <TableCell>
                            <div className="font-medium">{item.name}</div>
                            {item.builtin ? (
                              <span className="text-xs text-muted-foreground">
                                {t("内置策略")}
                              </span>
                            ) : null}
                          </TableCell>
                          <TableCell>
                            <code
                              className="block max-w-xl truncate text-xs"
                              title={item.pattern}
                            >
                              {item.pattern}
                            </code>
                          </TableCell>
                          <TableCell>
                            {item.action === "ban"
                              ? t("IP 封禁 · {value0}", {
                                  value0: durationLabel(
                                    item.ban_duration_seconds,
                                  ),
                                })
                              : t("仅拦截")}
                          </TableCell>
                          <TableCell>{formatNumber(item.priority)}</TableCell>
                          <TableCell>
                            <StatusBadge
                              status={item.enabled ? "succeeded" : "pending"}
                              label={item.enabled ? t("已启用") : t("已停用")}
                            />
                          </TableCell>
                          <TableCell>
                            <div className="flex justify-end gap-1">
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                aria-label={t("编辑策略")}
                                onClick={() => setPolicy(item)}
                              >
                                <Pencil />
                              </Button>
                              {!item.builtin ? (
                                <Button
                                  variant="ghost"
                                  size="icon-sm"
                                  aria-label={t("删除策略")}
                                  onClick={() =>
                                    setRemove({
                                      kind: "policy",
                                      id: item.id,
                                      name: item.name,
                                    })
                                  }
                                >
                                  <Trash2 />
                                </Button>
                              ) : null}
                            </div>
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </DataFrame>
              </TabsContent>
              <TabsContent value="rate">
                <SectionHeader
                  title={t("通用速率限制")}
                  meta={t("边缘节点按客户端 IP 执行一秒窗口限速")}
                  action={
                    <Button size="sm" onClick={() => setRateLimit("new")}>
                      <CirclePlus />
                      {t("新增")}
                    </Button>
                  }
                />
                <DataFrame
                  empty={!data.rate_limit_policies.length}
                  emptyTitle={t("暂无限速策略")}
                  footer={
                    <ListPagination
                      pagination={rateLimitPagination}
                      itemLabel={t("个策略")}
                    />
                  }
                >
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>{t("策略")}</TableHead>
                        <TableHead>{t("计数 Key")}</TableHead>
                        <TableHead>{t("阈值")}</TableHead>
                        <TableHead>{t("响应条件")}</TableHead>
                        <TableHead>{t("升级动作")}</TableHead>
                        <TableHead>{t("状态")}</TableHead>
                        <TableHead className="text-right">
                          {t("操作")}
                        </TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {rateLimitPagination.items.map((item) => (
                        <TableRow key={item.id}>
                          <TableCell className="font-medium">
                            {item.name}
                          </TableCell>
                          <TableCell>{t("客户端 IP")}</TableCell>
                          <TableCell className="tabular-nums">
                            {formatNumber(item.requests_per_second)}
                            {t(" 请求/秒")}
                          </TableCell>
                          <TableCell>
                            {item.response_condition_enabled
                              ? item.response_status_classes
                                  ?.map((code) => `${code}xx`)
                                  .join("、") || t("无有效条件")
                              : t("全部请求")}
                          </TableCell>
                          <TableCell>
                            {item.ban_enabled
                              ? t("连续 {value0} 次 429 · 封禁 {value1}", {
                                  value0: formatNumber(
                                    item.ban_after_consecutive_429,
                                  ),
                                  value1: durationLabel(
                                    item.ban_duration_seconds,
                                  ),
                                })
                              : t("仅返回 429")}
                          </TableCell>
                          <TableCell>
                            <StatusBadge
                              status={item.enabled ? "succeeded" : "pending"}
                              label={item.enabled ? t("已启用") : t("已停用")}
                            />
                          </TableCell>
                          <TableCell>
                            <div className="flex justify-end gap-1">
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                aria-label={t("编辑限速策略")}
                                onClick={() => setRateLimit(item)}
                              >
                                <Pencil />
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                aria-label={t("删除限速策略")}
                                onClick={() =>
                                  setRemove({
                                    kind: "rate",
                                    id: item.id,
                                    name: item.name,
                                  })
                                }
                              >
                                <Trash2 />
                              </Button>
                            </div>
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </DataFrame>
              </TabsContent>
              <TabsContent value="bans">
                <SectionHeader
                  title={t("活动封禁")}
                  meta={
                    data.active_ban_count > data.bans.length
                      ? t("共 {value0} 条，显示前 {value1} 条", {
                          value0: data.active_ban_count,
                          value1: data.bans.length,
                        })
                      : t("{value0} 条", {
                          value0: data.active_ban_count,
                        })
                  }
                />
                <DataFrame
                  empty={!data.bans.length}
                  emptyTitle={t("暂无活动封禁")}
                  footer={
                    <ListPagination
                      pagination={bansPagination}
                      itemLabel={t("个封禁")}
                    />
                  }
                >
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>IP</TableHead>
                        <TableHead>{t("触发策略")}</TableHead>
                        <TableHead>{t("节点")}</TableHead>
                        <TableHead>{t("请求")}</TableHead>
                        <TableHead>{t("到期时间")}</TableHead>
                        <TableHead className="text-right">
                          {t("操作")}
                        </TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {bansPagination.items.map((ban) => (
                        <TableRow key={ban.ip}>
                          <TableCell>
                            <code>{ban.ip}</code>
                          </TableCell>
                          <TableCell>{ban.policy_name || "--"}</TableCell>
                          <TableCell>
                            {nodeName(data, ban.trigger_node_id)}
                          </TableCell>
                          <TableCell>
                            <div className="text-xs">
                              {ban.method || "--"} · {ban.host || "--"}
                            </div>
                            <code className="text-xs">{ban.path || "--"}</code>
                          </TableCell>
                          <TableCell className="whitespace-nowrap text-xs">
                            {formatDateTime(ban.expires_at)}
                          </TableCell>
                          <TableCell className="text-right">
                            <Button
                              variant="outline"
                              size="sm"
                              disabled={unban.isPending}
                              onClick={() => unban.mutate(ban.ip)}
                            >
                              <LockOpen />
                              {t("解封")}
                            </Button>
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </DataFrame>
              </TabsContent>
              <TabsContent value="events">
                <SectionHeader
                  title={t("最近命中")}
                  meta={t("保留 7 天，每页最多 20 条")}
                />
                <DataFrame
                  empty={!data.events.length}
                  emptyTitle={t("暂无策略命中")}
                  footer={
                    <ListPagination
                      pagination={eventsPagination}
                      itemLabel={t("条命中")}
                    />
                  }
                >
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>{t("时间")}</TableHead>
                        <TableHead>IP</TableHead>
                        <TableHead>{t("策略")}</TableHead>
                        <TableHead>{t("节点")}</TableHead>
                        <TableHead>{t("请求")}</TableHead>
                        <TableHead>{t("动作")}</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {eventsPagination.items.map((event, index) => (
                        <TableRow
                          key={event.id || `${event.observed_at}-${index}`}
                        >
                          <TableCell className="whitespace-nowrap text-xs">
                            {formatDateTime(event.observed_at)}
                          </TableCell>
                          <TableCell>
                            <code>{event.client_ip}</code>
                          </TableCell>
                          <TableCell>{event.policy_name || "--"}</TableCell>
                          <TableCell>{nodeName(data, event.node_id)}</TableCell>
                          <TableCell>
                            <div className="text-xs">
                              {event.method || "--"} · {event.host || "--"}
                            </div>
                            <code className="text-xs">{event.path}</code>
                          </TableCell>
                          <TableCell>
                            {event.action === "ban"
                              ? t("IP 封禁")
                              : t("仅拦截")}
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </DataFrame>
              </TabsContent>
              <TabsContent value="nodes">
                <SectionHeader
                  title={t("节点部署")}
                  meta={t("能力与策略应用版本")}
                />
                <DataFrame
                  empty={!data.nodes.length}
                  emptyTitle={t("暂无节点")}
                  footer={
                    <ListPagination
                      pagination={nodesPagination}
                      itemLabel={t("个节点")}
                    />
                  }
                >
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>{t("节点")}</TableHead>
                        <TableHead>{t("状态")}</TableHead>
                        <TableHead>{t("访问能力")}</TableHead>
                        <TableHead>{t("限速能力")}</TableHead>
                        <TableHead>{t("版本")}</TableHead>
                        <TableHead>{t("结果")}</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {nodesPagination.items.map((node) => {
                        const ready =
                          node.configured &&
                          node.rate_limit_configured &&
                          node.desired_version > 0 &&
                          node.applied_version >= node.desired_version;
                        return (
                          <TableRow key={node.id}>
                            <TableCell className="font-medium">
                              {node.name}
                            </TableCell>
                            <TableCell>
                              <StatusBadge status={node.status} />
                            </TableCell>
                            <TableCell>
                              {node.capable
                                ? node.configured
                                  ? t("已配置")
                                  : t("待配置")
                                : t("需升级")}
                            </TableCell>
                            <TableCell>
                              {node.rate_limit_capable
                                ? node.rate_limit_configured
                                  ? t("已配置")
                                  : t("待配置")
                                : t("需升级")}
                            </TableCell>
                            <TableCell className="text-xs">
                              {t("期望 v")}
                              {node.desired_version}
                              {t(" · 当前 v")}
                              {node.applied_version}
                            </TableCell>
                            <TableCell>
                              {node.last_error ? (
                                <StatusBadge
                                  status="failed"
                                  label={t("节点错误")}
                                />
                              ) : ready ? (
                                <StatusBadge
                                  status="succeeded"
                                  label={t("已应用")}
                                />
                              ) : (
                                <StatusBadge
                                  status="applying"
                                  label={t("等待应用")}
                                />
                              )}
                            </TableCell>
                          </TableRow>
                        );
                      })}
                    </TableBody>
                  </Table>
                </DataFrame>
              </TabsContent>
            </Tabs>
          </>
        ) : null}
      </PageBody>
      <SecurityPolicyDialog
        value={policy}
        onOpenChange={(open) => {
          if (!open) setPolicy(null);
        }}
        onSaved={applyResult}
      />
      <RateLimitDialog
        value={rateLimit}
        onOpenChange={(open) => {
          if (!open) setRateLimit(null);
        }}
        onSaved={applyResult}
      />
      <ConfirmDialog
        open={Boolean(remove)}
        onOpenChange={(open) => {
          if (!open) setRemove(null);
        }}
        title={t("删除安全策略")}
        description={t("删除「{value0}」后会立即重新发布所有边缘配置。", {
          value0: remove?.name ?? "",
        })}
        confirmLabel={t("删除并发布")}
        destructive
        busy={removeMutation.isPending}
        onConfirm={async () => {
          if (remove) await removeMutation.mutateAsync(remove);
        }}
      />
    </>
  );
}
function SecurityPolicyDialog({
  value,
  onOpenChange,
  onSaved,
}: {
  value: SecurityPolicy | "new" | null;
  onOpenChange: (open: boolean) => void;
  onSaved: (data: SecurityOverview) => void;
}) {
  const existing = value && value !== "new" ? value : null;
  return (
    <PolicyDialogShell
      key={existing?.id || String(value)}
      open={Boolean(value)}
      title={existing ? t("编辑访问策略") : t("新增访问策略")}
      description={t("PCRE 兼容安全子集，保存后自动发布")}
      onOpenChange={onOpenChange}
      existing={existing}
      onSaved={onSaved}
    />
  );
}
function PolicyDialogShell({
  open,
  title,
  description,
  onOpenChange,
  existing,
  onSaved,
}: {
  open: boolean;
  title: string;
  description: string;
  onOpenChange: (open: boolean) => void;
  existing: SecurityPolicy | null;
  onSaved: (data: SecurityOverview) => void;
}) {
  const [name, setName] = useState(existing?.name ?? "");
  const [priority, setPriority] = useState(existing?.priority ?? 100);
  const [action, setAction] = useState<"block" | "ban">(
    existing?.action ?? "ban",
  );
  const [duration, setDuration] = useState(
    existing?.ban_duration_seconds ?? 21600,
  );
  const [enabled, setEnabled] = useState(existing?.enabled ?? true);
  const [pattern, setPattern] = useState(existing?.pattern ?? "");
  const mutation = useMutation({
    mutationFn: () =>
      api<SecurityOverview>(
        existing
          ? `/api/security/policies/${encodeURIComponent(existing.id)}`
          : "/api/security/policies",
        {
          method: existing ? "PUT" : "POST",
          body: JSON.stringify({
            name,
            enabled,
            pattern,
            action,
            ban_duration_seconds: action === "ban" ? duration : 0,
            priority,
          }),
        },
      ),
    onSuccess: (data) => {
      onSaved(data);
      onOpenChange(false);
      toast.success(t("访问策略已保存并发布"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    mutation.mutate();
  }
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            <DialogDescription>{description}</DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-5 sm:grid-cols-2">
            <Field label={t("名称")} id="policy-name">
              <Input
                id="policy-name"
                required
                maxLength={80}
                value={name}
                onChange={(event) => setName(event.target.value)}
              />
            </Field>
            <Field label={t("优先级")} id="policy-priority">
              <Input
                id="policy-priority"
                type="number"
                min={1}
                max={10000}
                required
                value={priority}
                onChange={(event) => setPriority(Number(event.target.value))}
              />
            </Field>
            <Field label={t("动作")} id="policy-action">
              <Select
                value={action}
                onValueChange={(value) => setAction(value as "block" | "ban")}
              >
                <SelectTrigger id="policy-action" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="ban">{t("IP 封禁")}</SelectItem>
                  <SelectItem value="block">{t("仅拦截请求")}</SelectItem>
                </SelectContent>
              </Select>
            </Field>
            {action === "ban" ? (
              <Field label={t("封禁时间")} id="policy-ban-duration">
                <Select
                  value={String(duration)}
                  onValueChange={(value) => setDuration(Number(value))}
                >
                  <SelectTrigger id="policy-ban-duration" className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="3600">{t("1 小时")}</SelectItem>
                    <SelectItem value="21600">{t("6 小时")}</SelectItem>
                    <SelectItem value="43200">{t("12 小时")}</SelectItem>
                    <SelectItem value="86400">{t("24 小时")}</SelectItem>
                    <SelectItem value="259200">{t("3 天")}</SelectItem>
                    <SelectItem value="604800">{t("7 天")}</SelectItem>
                  </SelectContent>
                </Select>
              </Field>
            ) : null}
            <div className="flex items-center justify-between rounded-lg border px-3 py-2 sm:col-span-2">
              <Label htmlFor="policy-enabled">{t("启用策略")}</Label>
              <Switch
                id="policy-enabled"
                checked={enabled}
                onCheckedChange={setEnabled}
              />
            </div>
            <div className="grid gap-2 sm:col-span-2">
              <Label htmlFor="policy-pattern">{t("路径正则")}</Label>
              <Textarea
                id="policy-pattern"
                required
                maxLength={2048}
                rows={7}
                spellCheck={false}
                className="font-mono text-xs"
                value={pattern}
                onChange={(event) => setPattern(event.target.value)}
              />
            </div>
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
            >
              {t("取消")}
            </Button>
            <Button type="submit" disabled={mutation.isPending}>
              {mutation.isPending ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <Rocket />
              )}
              {t("保存并发布")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
function RateLimitDialog({
  value,
  onOpenChange,
  onSaved,
}: {
  value: RateLimitPolicy | "new" | null;
  onOpenChange: (open: boolean) => void;
  onSaved: (data: SecurityOverview) => void;
}) {
  const existing = value && value !== "new" ? value : null;
  return (
    <RateDialogShell
      key={existing?.id || String(value)}
      open={Boolean(value)}
      existing={existing}
      onOpenChange={onOpenChange}
      onSaved={onSaved}
    />
  );
}
function RateDialogShell({
  open,
  existing,
  onOpenChange,
  onSaved,
}: {
  open: boolean;
  existing: RateLimitPolicy | null;
  onOpenChange: (open: boolean) => void;
  onSaved: (data: SecurityOverview) => void;
}) {
  const [name, setName] = useState(existing?.name ?? "");
  const [rps, setRPS] = useState(existing?.requests_per_second ?? 20);
  const [enabled, setEnabled] = useState(existing?.enabled ?? true);
  const [conditional, setConditional] = useState(
    existing?.response_condition_enabled ?? false,
  );
  const [classes, setClasses] = useState<number[]>(
    existing?.response_status_classes ?? [4, 5],
  );
  const [banEnabled, setBanEnabled] = useState(existing?.ban_enabled ?? false);
  const [banAfter, setBanAfter] = useState(
    existing?.ban_after_consecutive_429 ?? 3,
  );
  const [banDuration, setBanDuration] = useState(
    existing?.ban_duration_seconds ?? 3600,
  );
  const mutation = useMutation({
    mutationFn: () =>
      api<SecurityOverview>(
        existing
          ? `/api/security/rate-limit-policies/${encodeURIComponent(existing.id)}`
          : "/api/security/rate-limit-policies",
        {
          method: existing ? "PUT" : "POST",
          body: JSON.stringify({
            name,
            enabled,
            requests_per_second: rps,
            response_condition_enabled: conditional,
            response_status_classes: conditional ? classes : [],
            ban_enabled: banEnabled,
            ban_after_consecutive_429: banAfter,
            ban_duration_seconds: banDuration,
          }),
        },
      ),
    onSuccess: (data) => {
      onSaved(data);
      onOpenChange(false);
      toast.success(t("限速策略已保存并发布"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    mutation.mutate();
  }
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100vh-2rem)] overflow-y-auto sm:max-w-xl">
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>
              {existing ? t("编辑限速策略") : t("新增限速策略")}
            </DialogTitle>
            <DialogDescription>
              {t("边缘节点按客户端 IP 使用一秒窗口计数")}
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-5 sm:grid-cols-2">
            <Field label={t("名称")} id="rate-name">
              <Input
                id="rate-name"
                required
                maxLength={80}
                value={name}
                onChange={(event) => setName(event.target.value)}
              />
            </Field>
            <Field label={t("每秒请求上限")} id="rate-rps">
              <Input
                id="rate-rps"
                type="number"
                min={1}
                max={100000}
                required
                value={rps}
                onChange={(event) => setRPS(Number(event.target.value))}
              />
            </Field>
            <div className="flex items-center justify-between rounded-lg border px-3 py-2 sm:col-span-2">
              <Label htmlFor="rate-enabled">{t("启用策略")}</Label>
              <Switch
                id="rate-enabled"
                checked={enabled}
                onCheckedChange={setEnabled}
              />
            </div>
            <div className="flex items-center justify-between rounded-lg border px-3 py-2 sm:col-span-2">
              <div>
                <Label htmlFor="rate-conditional">{t("仅统计指定响应")}</Label>
                <p className="text-xs text-muted-foreground">
                  {t("根据响应状态类别计入窗口")}
                </p>
              </div>
              <Switch
                id="rate-conditional"
                checked={conditional}
                disabled={banEnabled}
                onCheckedChange={setConditional}
              />
            </div>
            {conditional ? (
              <div className="grid grid-cols-4 gap-2 sm:col-span-2">
                {[2, 3, 4, 5].map((code) => (
                  <label
                    key={code}
                    className="flex items-center gap-2 rounded-lg border px-3 py-2 text-sm"
                  >
                    <Checkbox
                      checked={classes.includes(code)}
                      disabled={banEnabled && code < 4}
                      onCheckedChange={(checked) =>
                        setClasses(
                          checked
                            ? [...classes, code].sort()
                            : classes.filter((item) => item !== code),
                        )
                      }
                    />
                    {code}xx
                  </label>
                ))}
              </div>
            ) : null}
            <div className="flex items-center justify-between rounded-lg border px-3 py-2 sm:col-span-2">
              <div>
                <Label htmlFor="rate-ban-enabled">
                  {t("连续超限后封禁 IP")}
                </Label>
                <p className="text-xs text-muted-foreground">
                  {t("达到连续 429 次数后触发边缘封禁")}
                </p>
              </div>
              <Switch
                id="rate-ban-enabled"
                checked={banEnabled}
                onCheckedChange={(checked) => {
                  setBanEnabled(checked);
                  if (checked) {
                    setConditional(true);
                    setClasses((current) => {
                      const errorClasses = current.filter(
                        (code) => code === 4 || code === 5,
                      );
                      return errorClasses.length ? errorClasses : [4, 5];
                    });
                  }
                }}
              />
            </div>
            {banEnabled ? (
              <>
                <Field label={t("连续 429 次数")} id="rate-ban-after">
                  <Input
                    id="rate-ban-after"
                    type="number"
                    min={1}
                    max={100}
                    required
                    value={banAfter}
                    onChange={(event) =>
                      setBanAfter(Number(event.target.value))
                    }
                  />
                </Field>
                <Field label={t("封禁时间")} id="rate-ban-duration">
                  <Select
                    value={String(banDuration)}
                    onValueChange={(value) => setBanDuration(Number(value))}
                  >
                    <SelectTrigger id="rate-ban-duration" className="w-full">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="3600">{t("1 小时")}</SelectItem>
                      <SelectItem value="21600">{t("6 小时")}</SelectItem>
                      <SelectItem value="43200">{t("12 小时")}</SelectItem>
                      <SelectItem value="86400">{t("24 小时")}</SelectItem>
                      <SelectItem value="259200">{t("3 天")}</SelectItem>
                      <SelectItem value="604800">{t("7 天")}</SelectItem>
                    </SelectContent>
                  </Select>
                </Field>
              </>
            ) : null}
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
              disabled={
                mutation.isPending ||
                (conditional && !classes.length) ||
                (banEnabled &&
                  (!conditional ||
                    classes.some((code) => code !== 4 && code !== 5) ||
                    banAfter < 1 ||
                    banAfter > 100))
              }
            >
              {mutation.isPending ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <Rocket />
              )}
              {t("保存并发布")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
function Summary({
  icon: Icon,
  label,
  value,
}: {
  icon: typeof ShieldCheck;
  label: string;
  value: string;
}) {
  return (
    <Card>
      <CardContent className="flex items-start justify-between p-5">
        <div>
          <p className="text-sm text-muted-foreground">{label}</p>
          <p className="mt-2 text-2xl font-semibold tabular-nums">{value}</p>
        </div>
        <Icon className="size-4 text-info" />
      </CardContent>
    </Card>
  );
}
function SectionHeader({
  title,
  meta,
  action,
}: {
  title: string;
  meta: string;
  action?: React.ReactNode;
}) {
  return (
    <div className="mb-3 flex items-center justify-between gap-3">
      <div>
        <h2 className="text-base font-semibold">{title}</h2>
        <p className="text-xs text-muted-foreground">{meta}</p>
      </div>
      {action}
    </div>
  );
}
function DataFrame({
  empty,
  emptyTitle,
  footer,
  children,
}: {
  empty: boolean;
  emptyTitle: string;
  footer?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <Panel>
      {empty ? (
        <div className="p-5">
          <EmptyState title={emptyTitle} />
        </div>
      ) : (
        <>
          {children}
          {footer}
        </>
      )}
    </Panel>
  );
}
function Field({
  label,
  id,
  children,
}: {
  label: string;
  id: string;
  children: React.ReactNode;
}) {
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  );
}
function durationLabel(seconds?: number) {
  return (
    (
      {
        3600: t("1 小时"),
        21600: t("6 小时"),
        43200: t("12 小时"),
        86400: t("24 小时"),
        259200: t("3 天"),
        604800: t("7 天"),
      } as Record<number, string>
    )[Number(seconds)] ?? "--"
  );
}
function nodeName(data: SecurityOverview, id?: string) {
  return data.nodes.find((node) => node.id === id)?.name || id || "--";
}
