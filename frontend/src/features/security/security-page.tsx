import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowDown,
  ArrowUp,
  Ban,
  CirclePlus,
  Fingerprint,
  LoaderCircle,
  LockOpen,
  Pencil,
  Rocket,
  ShieldCheck,
  Trash2,
  X,
  Zap,
} from "lucide-react";
import { useState, type FormEvent, type ReactNode } from "react";
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
import { useListPagination } from "@/hooks/use-list-pagination";
import { usePersistentEnum } from "@/hooks/use-persistent-state";
import { api, errorMessage } from "@/lib/api";
import { formatDateTime, formatDuration, formatNumber } from "@/lib/format";
import { t, useI18n } from "@/lib/i18n";
import type {
  POWPolicy,
  RateLimitPolicy,
  SecurityCondition,
  SecurityConditionField,
  SecurityConditionOperator,
  SecurityOverview,
  SecurityPolicy,
  SecurityPolicyAction,
  SecuritySiteOption,
} from "@/lib/types";

type RemoveTarget = {
  kind: "policy" | "pow" | "rate";
  id: string;
  name: string;
};

const conditionFields: SecurityConditionField[] = [
  "path",
  "raw_uri",
  "query",
  "method",
  "host",
  "user_agent",
  "client_ip",
  "header",
  "body",
];

const conditionOperators: SecurityConditionOperator[] = [
  "regex",
  "equals",
  "contains",
  "prefix",
  "suffix",
  "cidr",
];

export function SecurityPage() {
  useI18n();
  const queryClient = useQueryClient();
  const [section, setSection] = usePersistentEnum(
    "simple-cdn.security.tab",
    ["policies", "pow", "rate", "bans", "events", "nodes"] as const,
    "policies",
  );
  const [policy, setPolicy] = useState<SecurityPolicy | "new" | null>(null);
  const [powPolicy, setPOWPolicy] = useState<POWPolicy | "new" | null>(null);
  const [rateLimit, setRateLimit] = useState<RateLimitPolicy | "new" | null>(
    null,
  );
  const [remove, setRemove] = useState<RemoveTarget | null>(null);
  const query = useQuery({
    queryKey: ["security"],
    queryFn: () => api<SecurityOverview>("/api/security"),
    refetchInterval: 15_000,
  });
  const applyResult = (data: SecurityOverview) =>
    queryClient.setQueryData(["security"], data);
  const deploy = useMutation({
    mutationFn: () =>
      api<SecurityOverview>("/api/security/deploy", { method: "POST" }),
    onSuccess: (data) => {
      applyResult(data);
      toast.success(t("安全策略已重新发布"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const move = useMutation({
    mutationFn: ({ id, direction }: { id: string; direction: "up" | "down" }) =>
      api<SecurityOverview>(
        `/api/security/policies/${encodeURIComponent(id)}/move`,
        { method: "POST", body: JSON.stringify({ direction }) },
      ),
    onSuccess: (data) => applyResult(data),
    onError: (error) => toast.error(errorMessage(error)),
  });
  const removeMutation = useMutation({
    mutationFn: (target: RemoveTarget) =>
      api<SecurityOverview>(securityDeletePath(target), { method: "DELETE" }),
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
  const powPagination = useListPagination(data?.pow_policies ?? []);
  const rateLimitPagination = useListPagination(
    data?.rate_limit_policies ?? [],
  );
  const bansPagination = useListPagination(data?.bans ?? []);
  const eventsPagination = useListPagination(data?.events ?? []);
  const nodesPagination = useListPagination(data?.nodes ?? []);
  const enabled = data
    ? data.policies.filter((item) => item.enabled).length +
      data.pow_policies.filter((item) => item.enabled).length +
      data.rate_limit_policies.filter((item) => item.enabled).length
    : 0;
  const eligibleNodes =
    data?.nodes.filter((node) =>
      ["active", "draining"].includes(node.status),
    ) ?? [];
  const modernNodes = eligibleNodes.filter(
    (node) => node.waf_chain_capable && node.pow_capable,
  );
  const appliedNodes = modernNodes.filter(
    (node) =>
      node.configured &&
      node.pow_configured &&
      node.desired_version > 0 &&
      node.applied_version >= node.desired_version,
  );

  return (
    <>
      <PageHeader
        title={t("安全")}
        description={t("WAF 处理链、浏览器验证、请求限速与活动封禁")}
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
              {t("WAF 规则")}
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
                label={t("现代安全能力")}
                value={`${modernNodes.length} / ${eligibleNodes.length}`}
              />
              <Summary
                icon={Rocket}
                label={t("已应用节点")}
                value={`${appliedNodes.length} / ${modernNodes.length}`}
              />
            </section>
            <Tabs
              value={section}
              onValueChange={(value) => setSection(value as typeof section)}
              className="space-y-4"
            >
              <TabsList className="!grid !h-auto !w-full grid-cols-3 justify-start gap-1 sm:!inline-flex sm:!h-8 sm:!w-fit sm:flex-nowrap">
                <TabsTrigger className="!h-8" value="policies">
                  {t("WAF 处理链")}
                </TabsTrigger>
                <TabsTrigger className="!h-8" value="pow">
                  {t("浏览器验证")}
                </TabsTrigger>
                <TabsTrigger className="!h-8" value="rate">
                  {t("请求限速")}
                </TabsTrigger>
                <TabsTrigger className="!h-8" value="bans">
                  {t("活动封禁")}
                </TabsTrigger>
                <TabsTrigger className="!h-8" value="events">
                  {t("最近命中")}
                </TabsTrigger>
                <TabsTrigger className="!h-8" value="nodes">
                  {t("节点覆盖")}
                </TabsTrigger>
              </TabsList>

              <TabsContent value="policies">
                <SectionHeader
                  title={t("WAF 处理链")}
                  meta={t("边缘节点按链路顺序执行，规则内条件同时匹配")}
                  action={
                    <Button size="sm" onClick={() => setPolicy("new")}>
                      <CirclePlus />
                      {t("新增")}
                    </Button>
                  }
                />
                <DataFrame
                  empty={!data.policies.length}
                  emptyTitle={t("暂无 WAF 规则")}
                  footer={
                    <ListPagination
                      pagination={policiesPagination}
                      itemLabel={t("个规则")}
                    />
                  }
                >
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead className="w-20">{t("链路")}</TableHead>
                        <TableHead>{t("规则")}</TableHead>
                        <TableHead>{t("作用域")}</TableHead>
                        <TableHead>{t("匹配条件")}</TableHead>
                        <TableHead>{t("动作")}</TableHead>
                        <TableHead>{t("状态")}</TableHead>
                        <TableHead className="text-right">
                          {t("操作")}
                        </TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {policiesPagination.items.map((item) => {
                        const position = data.policies.findIndex(
                          (candidate) => candidate.id === item.id,
                        );
                        return (
                          <TableRow key={item.id}>
                            <TableCell>
                              <div className="flex items-center gap-1">
                                <span className="w-6 text-center font-mono text-xs tabular-nums text-muted-foreground">
                                  {position + 1}
                                </span>
                                <div className="grid">
                                  <Button
                                    variant="ghost"
                                    size="icon-xs"
                                    aria-label={t("上移规则")}
                                    disabled={position <= 0 || move.isPending}
                                    onClick={() =>
                                      move.mutate({
                                        id: item.id,
                                        direction: "up",
                                      })
                                    }
                                  >
                                    <ArrowUp />
                                  </Button>
                                  <Button
                                    variant="ghost"
                                    size="icon-xs"
                                    aria-label={t("下移规则")}
                                    disabled={
                                      position < 0 ||
                                      position >= data.policies.length - 1 ||
                                      move.isPending
                                    }
                                    onClick={() =>
                                      move.mutate({
                                        id: item.id,
                                        direction: "down",
                                      })
                                    }
                                  >
                                    <ArrowDown />
                                  </Button>
                                </div>
                              </div>
                            </TableCell>
                            <TableCell>
                              <div className="font-medium">{item.name}</div>
                              <div className="text-xs text-muted-foreground">
                                {item.builtin ? t("内置规则") : t("自定义规则")}
                                {" · "}
                                {t("优先级 {value0}", {
                                  value0: formatNumber(item.priority),
                                })}
                              </div>
                            </TableCell>
                            <TableCell className="max-w-52">
                              {scopeLabel(item.site_ids, data.sites)}
                            </TableCell>
                            <TableCell className="max-w-md">
                              <ConditionSummary conditions={item.conditions} />
                            </TableCell>
                            <TableCell>{actionLabel(item)}</TableCell>
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
                                  aria-label={t("编辑规则")}
                                  onClick={() => setPolicy(item)}
                                >
                                  <Pencil />
                                </Button>
                                {!item.builtin ? (
                                  <Button
                                    variant="ghost"
                                    size="icon-sm"
                                    aria-label={t("删除规则")}
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
                        );
                      })}
                    </TableBody>
                  </Table>
                </DataFrame>
              </TabsContent>

              <TabsContent value="pow">
                <SectionHeader
                  title={t("浏览器 PoW 验证")}
                  meta={t("按站点与路径触发无状态工作量证明挑战")}
                  action={
                    <Button size="sm" onClick={() => setPOWPolicy("new")}>
                      <CirclePlus />
                      {t("新增")}
                    </Button>
                  }
                />
                <DataFrame
                  empty={!data.pow_policies.length}
                  emptyTitle={t("暂无浏览器验证策略")}
                  footer={
                    <ListPagination
                      pagination={powPagination}
                      itemLabel={t("个策略")}
                    />
                  }
                >
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>{t("策略")}</TableHead>
                        <TableHead>{t("站点")}</TableHead>
                        <TableHead>{t("路径表达式")}</TableHead>
                        <TableHead>{t("难度")}</TableHead>
                        <TableHead>{t("通过有效期")}</TableHead>
                        <TableHead>{t("状态")}</TableHead>
                        <TableHead className="text-right">
                          {t("操作")}
                        </TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {powPagination.items.map((item) => (
                        <TableRow key={item.id}>
                          <TableCell>
                            <div className="font-medium">{item.name}</div>
                            <div className="text-xs text-muted-foreground">
                              {t("优先级 {value0}", {
                                value0: formatNumber(item.priority),
                              })}
                            </div>
                          </TableCell>
                          <TableCell className="max-w-60">
                            {scopeLabel(item.site_ids, data.sites)}
                          </TableCell>
                          <TableCell>
                            <code
                              className="block max-w-72 truncate text-xs"
                              title={item.path_pattern}
                            >
                              {item.path_pattern}
                            </code>
                          </TableCell>
                          <TableCell className="tabular-nums">
                            {item.difficulty_bits} bits
                          </TableCell>
                          <TableCell>
                            {formatDuration(item.pass_ttl_seconds)}
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
                                aria-label={t("编辑浏览器验证策略")}
                                onClick={() => setPOWPolicy(item)}
                              >
                                <Pencil />
                              </Button>
                              <Button
                                variant="ghost"
                                size="icon-sm"
                                aria-label={t("删除浏览器验证策略")}
                                onClick={() =>
                                  setRemove({
                                    kind: "pow",
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
                      : t("{value0} 条", { value0: data.active_ban_count })
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
                        <TableHead>{t("站点 / 节点")}</TableHead>
                        <TableHead>{t("请求")}</TableHead>
                        <TableHead>{t("命中字段")}</TableHead>
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
                          <TableCell className="text-xs">
                            <div>{siteName(data, event.site_id)}</div>
                            <div className="text-muted-foreground">
                              {nodeName(data, event.node_id)}
                            </div>
                          </TableCell>
                          <TableCell className="max-w-80">
                            <div className="text-xs">
                              {event.method || "--"} · {event.host || "--"}
                            </div>
                            <code
                              className="block truncate text-xs"
                              title={event.raw_uri || event.path}
                            >
                              {event.raw_uri || event.path}
                            </code>
                          </TableCell>
                          <TableCell>
                            {event.matched_field
                              ? fieldLabel(event.matched_field)
                              : "--"}
                          </TableCell>
                          <TableCell>
                            {eventActionLabel(event.action)}
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
                  meta={t("安全能力与策略应用版本")}
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
                        <TableHead>{t("WAF 处理链")}</TableHead>
                        <TableHead>{t("浏览器验证")}</TableHead>
                        <TableHead>{t("请求限速")}</TableHead>
                        <TableHead>{t("版本")}</TableHead>
                        <TableHead>{t("结果")}</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {nodesPagination.items.map((node) => {
                        const ready =
                          node.configured &&
                          (!node.pow_capable || node.pow_configured) &&
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
                              {node.waf_chain_capable
                                ? node.configured
                                  ? t("已配置")
                                  : t("待配置")
                                : node.capable
                                  ? t("兼容模式")
                                  : t("需升级")}
                            </TableCell>
                            <TableCell>
                              {node.pow_capable
                                ? node.pow_configured
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
                            <TableCell className="whitespace-nowrap text-xs">
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
        sites={data?.sites ?? []}
        onOpenChange={(open) => {
          if (!open) setPolicy(null);
        }}
        onSaved={applyResult}
      />
      <POWPolicyDialog
        value={powPolicy}
        sites={data?.sites ?? []}
        onOpenChange={(open) => {
          if (!open) setPOWPolicy(null);
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
  sites,
  onOpenChange,
  onSaved,
}: {
  value: SecurityPolicy | "new" | null;
  sites: SecuritySiteOption[];
  onOpenChange: (open: boolean) => void;
  onSaved: (data: SecurityOverview) => void;
}) {
  const existing = value && value !== "new" ? value : null;
  return (
    <PolicyDialogShell
      key={existing?.id || String(value)}
      open={Boolean(value)}
      existing={existing}
      sites={sites}
      onOpenChange={onOpenChange}
      onSaved={onSaved}
    />
  );
}

function PolicyDialogShell({
  open,
  existing,
  sites,
  onOpenChange,
  onSaved,
}: {
  open: boolean;
  existing: SecurityPolicy | null;
  sites: SecuritySiteOption[];
  onOpenChange: (open: boolean) => void;
  onSaved: (data: SecurityOverview) => void;
}) {
  const [name, setName] = useState(existing?.name ?? "");
  const [priority, setPriority] = useState(existing?.priority ?? 1000);
  const [action, setAction] = useState<SecurityPolicyAction>(
    existing?.action ?? "block",
  );
  const [duration, setDuration] = useState(
    existing?.ban_duration_seconds ?? 21600,
  );
  const [responseStatus, setResponseStatus] = useState(
    existing?.response_status ?? 403,
  );
  const [enabled, setEnabled] = useState(existing?.enabled ?? true);
  const [siteIDs, setSiteIDs] = useState(existing?.site_ids ?? []);
  const [conditions, setConditions] = useState<SecurityCondition[]>(
    existing?.conditions?.length
      ? existing.conditions.map((condition) => ({ ...condition }))
      : [
          {
            field: "path",
            operator: "regex",
            value: existing?.pattern ?? "",
          },
        ],
  );
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
            site_ids: siteIDs,
            conditions,
            pattern: "",
            action,
            ban_duration_seconds: action === "ban" ? duration : 0,
            response_status:
              action === "block" || action === "ban" ? responseStatus : 0,
            priority,
          }),
        },
      ),
    onSuccess: (data) => {
      onSaved(data);
      onOpenChange(false);
      toast.success(t("WAF 规则已保存并发布"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const invalid =
    !name.trim() ||
    priority < 1 ||
    priority > 10000 ||
    !conditions.length ||
    conditions.length > 8 ||
    conditions.some(
      (condition) =>
        !condition.value.trim() ||
        (condition.field === "header" && !condition.header_name?.trim()) ||
        (condition.operator === "cidr" && condition.field !== "client_ip"),
    );
  function submit(event: FormEvent) {
    event.preventDefault();
    if (!invalid) mutation.mutate();
  }
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100vh-2rem)] overflow-y-auto sm:max-w-4xl">
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>
              {existing ? t("编辑 WAF 规则") : t("新增 WAF 规则")}
            </DialogTitle>
            <DialogDescription>
              {t("按优先级加入边缘 WAF 处理链")}
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-5 py-5">
            <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
              <Field
                label={t("名称")}
                id="policy-name"
                className="sm:col-span-2"
              >
                <Input
                  id="policy-name"
                  required
                  maxLength={80}
                  value={name}
                  onChange={(event) => setName(event.target.value)}
                />
              </Field>
              <Field label={t("动作")} id="policy-action">
                <Select
                  value={action}
                  onValueChange={(value) =>
                    setAction(value as SecurityPolicyAction)
                  }
                >
                  <SelectTrigger id="policy-action" className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="allow">{t("立即放行")}</SelectItem>
                    <SelectItem value="log">{t("仅记录")}</SelectItem>
                    <SelectItem value="block">{t("拦截请求")}</SelectItem>
                    <SelectItem value="ban">{t("拦截并封禁 IP")}</SelectItem>
                  </SelectContent>
                </Select>
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
              {(action === "block" || action === "ban") && (
                <Field label={t("拦截响应")} id="policy-response-status">
                  <Select
                    value={String(responseStatus)}
                    onValueChange={(value) => setResponseStatus(Number(value))}
                  >
                    <SelectTrigger
                      id="policy-response-status"
                      className="w-full"
                    >
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="403">403 Forbidden</SelectItem>
                      <SelectItem value="404">404 Not Found</SelectItem>
                      <SelectItem value="444">{t("444 关闭连接")}</SelectItem>
                    </SelectContent>
                  </Select>
                </Field>
              )}
              {action === "ban" ? (
                <Field label={t("封禁时间")} id="policy-ban-duration">
                  <DurationSelect
                    id="policy-ban-duration"
                    value={duration}
                    onChange={setDuration}
                  />
                </Field>
              ) : null}
              <div className="flex items-center justify-between rounded-lg border px-3 py-2 sm:col-span-2">
                <Label htmlFor="policy-enabled">{t("启用规则")}</Label>
                <Switch
                  id="policy-enabled"
                  checked={enabled}
                  onCheckedChange={setEnabled}
                />
              </div>
            </div>

            <div className="grid gap-2">
              <div className="flex items-center justify-between gap-3">
                <Label>{t("匹配条件")}</Label>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  disabled={conditions.length >= 8}
                  onClick={() =>
                    setConditions((current) => [
                      ...current,
                      { field: "path", operator: "contains", value: "" },
                    ])
                  }
                >
                  <CirclePlus />
                  {t("条件")}
                </Button>
              </div>
              <div className="grid gap-2">
                {conditions.map((condition, index) => (
                  <ConditionEditor
                    key={index}
                    index={index}
                    condition={condition}
                    removable={conditions.length > 1}
                    onChange={(next) =>
                      setConditions((current) =>
                        current.map((item, itemIndex) =>
                          itemIndex === index ? next : item,
                        ),
                      )
                    }
                    onRemove={() =>
                      setConditions((current) =>
                        current.filter((_, itemIndex) => itemIndex !== index),
                      )
                    }
                  />
                ))}
              </div>
            </div>

            <SiteScopeSelector
              sites={sites}
              selected={siteIDs}
              onChange={setSiteIDs}
              allowAll
            />
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
            >
              {t("取消")}
            </Button>
            <Button type="submit" disabled={mutation.isPending || invalid}>
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

function ConditionEditor({
  index,
  condition,
  removable,
  onChange,
  onRemove,
}: {
  index: number;
  condition: SecurityCondition;
  removable: boolean;
  onChange: (condition: SecurityCondition) => void;
  onRemove: () => void;
}) {
  const operators = conditionOperators.filter(
    (operator) => operator !== "cidr" || condition.field === "client_ip",
  );
  return (
    <div className="grid gap-3 rounded-lg border p-3">
      <div className="flex items-center justify-between">
        <span className="text-xs font-medium text-muted-foreground">
          {t("条件 {value0}", { value0: index + 1 })}
        </span>
        <Button
          type="button"
          variant="ghost"
          size="icon-xs"
          aria-label={t("删除条件")}
          disabled={!removable}
          onClick={onRemove}
        >
          <X />
        </Button>
      </div>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-[11rem_10rem_minmax(12rem,1fr)]">
        <Field label={t("字段")} id={`condition-field-${index}`}>
          <Select
            value={condition.field}
            onValueChange={(value) => {
              const field = value as SecurityConditionField;
              onChange({
                ...condition,
                field,
                operator:
                  condition.operator === "cidr" && field !== "client_ip"
                    ? "equals"
                    : condition.operator,
                header_name: field === "header" ? condition.header_name : "",
              });
            }}
          >
            <SelectTrigger id={`condition-field-${index}`} className="w-full">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {conditionFields.map((field) => (
                <SelectItem key={field} value={field}>
                  {fieldLabel(field)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
        <Field label={t("运算符")} id={`condition-operator-${index}`}>
          <Select
            value={condition.operator}
            onValueChange={(value) =>
              onChange({
                ...condition,
                operator: value as SecurityConditionOperator,
              })
            }
          >
            <SelectTrigger
              id={`condition-operator-${index}`}
              className="w-full"
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {operators.map((operator) => (
                <SelectItem key={operator} value={operator}>
                  {operatorLabel(operator)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
        <Field label={t("匹配值")} id={`condition-value-${index}`}>
          <Input
            id={`condition-value-${index}`}
            required
            maxLength={2048}
            spellCheck={false}
            className="font-mono text-xs"
            value={condition.value}
            onChange={(event) =>
              onChange({ ...condition, value: event.target.value })
            }
          />
        </Field>
      </div>
      <div className="flex flex-wrap items-end gap-5">
        {condition.field === "header" ? (
          <Field
            label={t("请求头名称")}
            id={`condition-header-${index}`}
            className="min-w-56 flex-1"
          >
            <Input
              id={`condition-header-${index}`}
              required
              maxLength={128}
              placeholder="X-Requested-With"
              value={condition.header_name ?? ""}
              onChange={(event) =>
                onChange({ ...condition, header_name: event.target.value })
              }
            />
          </Field>
        ) : null}
        <label className="flex h-8 items-center gap-2 text-sm">
          <Checkbox
            checked={Boolean(condition.negate)}
            onCheckedChange={(checked) =>
              onChange({ ...condition, negate: checked === true })
            }
          />
          {t("取反")}
        </label>
        <label className="flex h-8 items-center gap-2 text-sm">
          <Checkbox
            checked={Boolean(condition.case_sensitive)}
            disabled={condition.operator === "cidr"}
            onCheckedChange={(checked) =>
              onChange({ ...condition, case_sensitive: checked === true })
            }
          />
          {t("区分大小写")}
        </label>
      </div>
    </div>
  );
}

function POWPolicyDialog({
  value,
  sites,
  onOpenChange,
  onSaved,
}: {
  value: POWPolicy | "new" | null;
  sites: SecuritySiteOption[];
  onOpenChange: (open: boolean) => void;
  onSaved: (data: SecurityOverview) => void;
}) {
  const existing = value && value !== "new" ? value : null;
  return (
    <POWDialogShell
      key={existing?.id || String(value)}
      open={Boolean(value)}
      existing={existing}
      sites={sites}
      onOpenChange={onOpenChange}
      onSaved={onSaved}
    />
  );
}

function POWDialogShell({
  open,
  existing,
  sites,
  onOpenChange,
  onSaved,
}: {
  open: boolean;
  existing: POWPolicy | null;
  sites: SecuritySiteOption[];
  onOpenChange: (open: boolean) => void;
  onSaved: (data: SecurityOverview) => void;
}) {
  const [name, setName] = useState(existing?.name ?? "");
  const [siteIDs, setSiteIDs] = useState(existing?.site_ids ?? []);
  const [pathPattern, setPathPattern] = useState(
    existing?.path_pattern ?? "^/",
  );
  const [difficulty, setDifficulty] = useState(existing?.difficulty_bits ?? 18);
  const [challengeTTL, setChallengeTTL] = useState(
    existing?.challenge_ttl_seconds ?? 120,
  );
  const [passTTL, setPassTTL] = useState(existing?.pass_ttl_seconds ?? 1800);
  const [priority, setPriority] = useState(existing?.priority ?? 100);
  const [enabled, setEnabled] = useState(existing?.enabled ?? true);
  const mutation = useMutation({
    mutationFn: () =>
      api<SecurityOverview>(
        existing
          ? `/api/security/pow-policies/${encodeURIComponent(existing.id)}`
          : "/api/security/pow-policies",
        {
          method: existing ? "PUT" : "POST",
          body: JSON.stringify({
            name,
            enabled,
            site_ids: siteIDs,
            path_pattern: pathPattern,
            difficulty_bits: difficulty,
            challenge_ttl_seconds: challengeTTL,
            pass_ttl_seconds: passTTL,
            priority,
          }),
        },
      ),
    onSuccess: (data) => {
      onSaved(data);
      onOpenChange(false);
      toast.success(t("浏览器验证策略已保存并发布"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const invalid =
    !name.trim() ||
    !siteIDs.length ||
    !pathPattern.trim() ||
    difficulty < 16 ||
    difficulty > 24 ||
    challengeTTL < 30 ||
    challengeTTL > 600 ||
    passTTL < 300 ||
    passTTL > 86400 ||
    priority < 1 ||
    priority > 10000;
  function submit(event: FormEvent) {
    event.preventDefault();
    if (!invalid) mutation.mutate();
  }
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[calc(100vh-2rem)] overflow-y-auto sm:max-w-2xl">
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>
              {existing ? t("编辑浏览器验证策略") : t("新增浏览器验证策略")}
            </DialogTitle>
            <DialogDescription>
              {t("浏览器完成工作量证明后获得短期通行凭证")}
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-5 py-5">
            <div className="grid gap-4 sm:grid-cols-2">
              <Field label={t("名称")} id="pow-name">
                <Input
                  id="pow-name"
                  required
                  maxLength={80}
                  value={name}
                  onChange={(event) => setName(event.target.value)}
                />
              </Field>
              <Field label={t("优先级")} id="pow-priority">
                <Input
                  id="pow-priority"
                  type="number"
                  min={1}
                  max={10000}
                  required
                  value={priority}
                  onChange={(event) => setPriority(Number(event.target.value))}
                />
              </Field>
              <Field
                label={t("路径表达式")}
                id="pow-path"
                className="sm:col-span-2"
              >
                <Input
                  id="pow-path"
                  required
                  maxLength={2048}
                  spellCheck={false}
                  className="font-mono text-xs"
                  value={pathPattern}
                  onChange={(event) => setPathPattern(event.target.value)}
                />
              </Field>
              <Field label={t("计算难度（bits）")} id="pow-difficulty">
                <Input
                  id="pow-difficulty"
                  type="number"
                  min={16}
                  max={24}
                  required
                  value={difficulty}
                  onChange={(event) =>
                    setDifficulty(Number(event.target.value))
                  }
                />
              </Field>
              <Field label={t("挑战有效期（秒）")} id="pow-challenge-ttl">
                <Input
                  id="pow-challenge-ttl"
                  type="number"
                  min={30}
                  max={600}
                  required
                  value={challengeTTL}
                  onChange={(event) =>
                    setChallengeTTL(Number(event.target.value))
                  }
                />
              </Field>
              <Field label={t("通过有效期（秒）")} id="pow-pass-ttl">
                <Input
                  id="pow-pass-ttl"
                  type="number"
                  min={300}
                  max={86400}
                  required
                  value={passTTL}
                  onChange={(event) => setPassTTL(Number(event.target.value))}
                />
              </Field>
              <div className="flex items-center justify-between rounded-lg border px-3 py-2">
                <Label htmlFor="pow-enabled">{t("启用策略")}</Label>
                <Switch
                  id="pow-enabled"
                  checked={enabled}
                  onCheckedChange={setEnabled}
                />
              </div>
            </div>
            <SiteScopeSelector
              sites={sites}
              selected={siteIDs}
              onChange={setSiteIDs}
            />
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
            >
              {t("取消")}
            </Button>
            <Button type="submit" disabled={mutation.isPending || invalid}>
              {mutation.isPending ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <Fingerprint />
              )}
              {t("保存并发布")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function SiteScopeSelector({
  sites,
  selected,
  onChange,
  allowAll = false,
}: {
  sites: SecuritySiteOption[];
  selected: string[];
  onChange: (siteIDs: string[]) => void;
  allowAll?: boolean;
}) {
  return (
    <div className="grid gap-2">
      <Label>{t("作用站点")}</Label>
      <div className="max-h-52 overflow-y-auto rounded-lg border">
        {allowAll ? (
          <label className="flex items-center gap-3 border-b px-3 py-2.5 text-sm">
            <Checkbox
              checked={selected.length === 0}
              onCheckedChange={(checked) => {
                if (checked === true) onChange([]);
              }}
            />
            <span className="font-medium">{t("全部站点")}</span>
          </label>
        ) : null}
        {sites.length ? (
          sites.map((site) => (
            <label
              key={site.id}
              className="flex items-center justify-between gap-3 border-b px-3 py-2.5 text-sm last:border-b-0"
            >
              <span className="flex min-w-0 items-center gap-3">
                <Checkbox
                  checked={selected.includes(site.id)}
                  disabled={site.deleting}
                  onCheckedChange={(checked) => {
                    if (checked === true) {
                      onChange([...new Set([...selected, site.id])]);
                    } else {
                      onChange(selected.filter((id) => id !== site.id));
                    }
                  }}
                />
                <span className="min-w-0">
                  <span className="block font-medium">{site.name}</span>
                  <span className="block truncate text-xs text-muted-foreground">
                    {site.domains.join(", ") || "--"}
                  </span>
                </span>
              </span>
              {!site.enabled ? (
                <span className="shrink-0 text-xs text-muted-foreground">
                  {t("已停用")}
                </span>
              ) : null}
            </label>
          ))
        ) : (
          <div className="p-4 text-sm text-muted-foreground">
            {t("暂无站点")}
          </div>
        )}
      </div>
    </div>
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
            <ToggleRow
              id="rate-enabled"
              label={t("启用策略")}
              checked={enabled}
              onChange={setEnabled}
            />
            <ToggleRow
              id="rate-conditional"
              label={t("仅统计指定响应")}
              meta={t("根据响应状态类别计入窗口")}
              checked={conditional}
              disabled={banEnabled}
              onChange={setConditional}
            />
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
                          checked === true
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
            <ToggleRow
              id="rate-ban-enabled"
              label={t("连续超限后封禁 IP")}
              meta={t("达到连续 429 次数后触发边缘封禁")}
              checked={banEnabled}
              onChange={(checked) => {
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
                  <DurationSelect
                    id="rate-ban-duration"
                    value={banDuration}
                    onChange={setBanDuration}
                  />
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

function DurationSelect({
  id,
  value,
  onChange,
}: {
  id: string;
  value: number;
  onChange: (value: number) => void;
}) {
  return (
    <Select
      value={String(value)}
      onValueChange={(next) => onChange(Number(next))}
    >
      <SelectTrigger id={id} className="w-full">
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
  );
}

function ToggleRow({
  id,
  label,
  meta,
  checked,
  disabled,
  onChange,
}: {
  id: string;
  label: string;
  meta?: string;
  checked: boolean;
  disabled?: boolean;
  onChange: (checked: boolean) => void;
}) {
  return (
    <div className="flex items-center justify-between rounded-lg border px-3 py-2 sm:col-span-2">
      <div>
        <Label htmlFor={id}>{label}</Label>
        {meta ? <p className="text-xs text-muted-foreground">{meta}</p> : null}
      </div>
      <Switch
        id={id}
        checked={checked}
        disabled={disabled}
        onCheckedChange={onChange}
      />
    </div>
  );
}

function ConditionSummary({ conditions }: { conditions: SecurityCondition[] }) {
  if (!conditions.length) return <span>--</span>;
  return (
    <div className="grid gap-1">
      {conditions.slice(0, 2).map((condition, index) => (
        <code
          key={index}
          className="block truncate text-xs"
          title={conditionText(condition)}
        >
          {conditionText(condition)}
        </code>
      ))}
      {conditions.length > 2 ? (
        <span className="text-xs text-muted-foreground">
          {t("另有 {value0} 个条件", { value0: conditions.length - 2 })}
        </span>
      ) : null}
    </div>
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
  action?: ReactNode;
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
  footer?: ReactNode;
  children: ReactNode;
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
  className,
  children,
}: {
  label: string;
  id: string;
  className?: string;
  children: ReactNode;
}) {
  return (
    <div className={`grid gap-2 ${className ?? ""}`}>
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  );
}

function securityDeletePath(target: RemoveTarget) {
  const collection =
    target.kind === "policy"
      ? "policies"
      : target.kind === "pow"
        ? "pow-policies"
        : "rate-limit-policies";
  return `/api/security/${collection}/${encodeURIComponent(target.id)}`;
}

function fieldLabel(field: SecurityConditionField) {
  return (
    {
      path: t("规范化路径"),
      raw_uri: t("原始 URI"),
      query: t("查询字符串"),
      method: t("请求方法"),
      host: t("主机名"),
      user_agent: t("User-Agent"),
      client_ip: t("客户端 IP"),
      header: t("请求头"),
      body: t("请求体"),
    } satisfies Record<SecurityConditionField, string>
  )[field];
}

function operatorLabel(operator: SecurityConditionOperator) {
  return (
    {
      regex: t("正则匹配"),
      equals: t("等于"),
      contains: t("包含"),
      prefix: t("前缀"),
      suffix: t("后缀"),
      cidr: t("CIDR 网段"),
    } satisfies Record<SecurityConditionOperator, string>
  )[operator];
}

function conditionText(condition: SecurityCondition) {
  const field =
    condition.field === "header" && condition.header_name
      ? `${fieldLabel(condition.field)} ${condition.header_name}`
      : fieldLabel(condition.field);
  return `${condition.negate ? t("非") + " " : ""}${field} · ${operatorLabel(condition.operator)} · ${condition.value}`;
}

function actionLabel(policy: SecurityPolicy) {
  switch (policy.action) {
    case "allow":
      return t("立即放行");
    case "log":
      return t("仅记录");
    case "ban":
      return t("封禁 {value0} · HTTP {value1}", {
        value0: durationLabel(policy.ban_duration_seconds),
        value1: policy.response_status ?? 403,
      });
    default:
      return t("拦截 · HTTP {value0}", {
        value0: policy.response_status ?? 403,
      });
  }
}

function eventActionLabel(action: SecurityPolicyAction) {
  return (
    {
      allow: t("已放行"),
      log: t("已记录"),
      block: t("已拦截"),
      ban: t("IP 封禁"),
    } satisfies Record<SecurityPolicyAction, string>
  )[action];
}

function scopeLabel(
  siteIDs: string[] | undefined,
  sites: SecuritySiteOption[],
) {
  if (!siteIDs?.length) return t("全部站点");
  const names = siteIDs.map(
    (id) => sites.find((site) => site.id === id)?.name || id,
  );
  return names.length > 2
    ? t("{value0} 等 {value1} 个站点", {
        value0: names.slice(0, 2).join("、"),
        value1: names.length,
      })
    : names.join("、");
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

function siteName(data: SecurityOverview, id?: string) {
  return data.sites.find((site) => site.id === id)?.name || id || "--";
}
