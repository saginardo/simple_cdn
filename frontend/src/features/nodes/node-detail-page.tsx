import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowLeft,
  CirclePause,
  CirclePlay,
  Copy,
  HardDrive,
  KeyRound,
  LoaderCircle,
  RefreshCw,
  Rocket,
  Save,
  ShieldOff,
  Trash2,
  Unplug,
  Wifi,
} from "lucide-react";
import { useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/confirm-dialog";
import { CopyButton } from "@/components/copy-button";
import { ListPagination } from "@/components/list-pagination";
import {
  EmptyState,
  PageBody,
  PageError,
  PageHeader,
  PageLoading,
} from "@/components/page";
import { StatusBadge } from "@/components/status-badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
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
import { api, errorMessage } from "@/lib/api";
import { useListPagination } from "@/hooks/use-list-pagination";
import {
  formatBytes,
  formatDateTime,
  formatDuration,
  formatNumber,
  formatPercent,
  shortHash,
} from "@/lib/format";
import type {
  Node,
  NodeCacheSettings,
  NodeCacheStatus,
  NodeDetail,
  NginxCapacity,
  NodeStatus,
  NodeUninstallStatus,
} from "@/lib/types";

interface CommandResult {
  install_command: string;
  enrollment_required: boolean;
  expires_at?: string;
}

export function NodeDetailPage() {
  const { nodeId = "" } = useParams();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [command, setCommand] = useState("");
  const [confirm, setConfirm] = useState<"force" | "delete" | null>(null);
  const detail = useQuery({
    queryKey: ["node", nodeId],
    queryFn: () => api<NodeDetail>(`/api/nodes/${encodeURIComponent(nodeId)}`),
    refetchInterval: 10_000,
  });
  const cache = useQuery({
    queryKey: ["node-cache", nodeId],
    queryFn: () =>
      api<NodeCacheStatus>(
        `/api/nodes/${encodeURIComponent(nodeId)}/cache-status`,
      ),
    refetchInterval: 30_000,
  });
  const uninstall = useQuery({
    queryKey: ["node-uninstall", nodeId],
    queryFn: () =>
      api<NodeUninstallStatus>(
        `/api/nodes/${encodeURIComponent(nodeId)}/uninstall`,
      ),
    refetchInterval: (query) =>
      query.state.data?.job &&
      !["succeeded", "forced", "canceled"].includes(query.state.data.job.status)
        ? 5_000
        : false,
    retry: false,
  });
  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["node", nodeId] });
    void queryClient.invalidateQueries({ queryKey: ["nodes"] });
    void queryClient.invalidateQueries({
      queryKey: ["node-uninstall", nodeId],
    });
  };
  const node = detail.data?.node;

  const statusMutation = useMutation({
    mutationFn: (status: NodeStatus) =>
      api<{ ok: boolean; smart_routing_disabled: boolean }>(
        `/api/nodes/${encodeURIComponent(nodeId)}/status`,
        {
          method: "POST",
          body: JSON.stringify({ status }),
        },
      ),
    onSuccess: (result) => {
      toast.success(
        result.smart_routing_disabled
          ? "节点状态已更新，智能路由已关闭"
          : "节点状态已更新",
      );
      void queryClient.invalidateQueries({ queryKey: ["monitoring"] });
      refresh();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const commandMutation = useMutation({
    mutationFn: () =>
      api<CommandResult>(
        `/api/nodes/${encodeURIComponent(nodeId)}/enrollment-token`,
        { method: "POST" },
      ),
    onSuccess: (result) => setCommand(result.install_command),
    onError: (error) => toast.error(errorMessage(error)),
  });
  const upgrade = useMutation({
    mutationFn: () =>
      api<Node>(`/api/nodes/${encodeURIComponent(nodeId)}/upgrade`, {
        method: "POST",
      }),
    onSuccess: () => {
      toast.success("在线升级已启动");
      refresh();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const uninstallAction = useMutation({
    mutationFn: ({
      path,
      method = "POST",
      body,
    }: {
      path: string;
      method?: string;
      body?: string;
    }) => api<NodeUninstallStatus>(path, { method, body }),
    onSuccess: (result) => {
      if (result.uninstall_command) setCommand(result.uninstall_command);
      toast.success("卸载流程已更新");
      refresh();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const deleteNode = useMutation({
    mutationFn: () =>
      api(`/api/nodes/${encodeURIComponent(nodeId)}`, {
        method: "DELETE",
        body: JSON.stringify({ confirmation: node?.name }),
      }),
    onSuccess: () => {
      toast.success("节点记录已删除");
      void queryClient.invalidateQueries({ queryKey: ["nodes"] });
      navigate("/nodes");
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  return (
    <>
      <PageHeader
        title={node?.name ?? "节点详情"}
        description={
          node ? `${node.public_ipv4} · ${node.id}` : "节点运行状态与运维操作"
        }
        actions={
          <>
            <Button asChild variant="outline">
              <Link to="/nodes">
                <ArrowLeft />
                返回节点
              </Link>
            </Button>
            <Button
              variant="outline"
              size="icon"
              aria-label="刷新节点"
              onClick={refresh}
            >
              <RefreshCw />
            </Button>
          </>
        }
      />
      <PageBody>
        {detail.isLoading ? <PageLoading /> : null}
        {detail.error ? (
          <PageError title="节点加载失败" error={detail.error} />
        ) : null}
        {node && detail.data ? (
          <>
            <section className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_22rem]">
              <div className="space-y-4">
                <NodeSummary detail={detail.data} />
                <MachineStatus detail={detail.data} />
                <CacheStatus query={cache} />
                <AssignedSites sites={detail.data.sites} />
              </div>
              <div className="space-y-4">
                <CacheQuotaSettings
                  key={`${detail.data.cache.default_size_gb}-${detail.data.cache.override_size_gb ?? "global"}`}
                  nodeId={nodeId}
                  settings={detail.data.cache}
                />
                <NginxCapacitySettings
                  key={`${node.nginx_capacity.worker_processes}-${node.nginx_capacity.worker_connections}-${node.nginx_capacity.worker_rlimit_nofile}`}
                  nodeId={nodeId}
                  capacity={node.nginx_capacity}
                  capacitySupported={node.capabilities.includes(
                    "nginx_capacity_v1",
                  )}
                />
                <Card>
                  <CardHeader>
                    <CardTitle>节点操作</CardTitle>
                    <CardDescription>状态、部署与在线升级</CardDescription>
                  </CardHeader>
                  <CardContent className="grid gap-2">
                    {node.status === "active" ? (
                      <Button
                        variant="outline"
                        disabled={statusMutation.isPending}
                        onClick={() => statusMutation.mutate("draining")}
                      >
                        <CirclePause />
                        暂停调度
                      </Button>
                    ) : null}
                    {node.status === "draining" ? (
                      <Button
                        variant="outline"
                        disabled={statusMutation.isPending}
                        onClick={() => statusMutation.mutate("active")}
                      >
                        <CirclePlay />
                        恢复运行
                      </Button>
                    ) : null}
                    {["pending", "active", "draining"].includes(node.status) ? (
                      <Button
                        variant="outline"
                        disabled={statusMutation.isPending}
                        onClick={() => statusMutation.mutate("revoked")}
                      >
                        <ShieldOff />
                        撤销授权
                      </Button>
                    ) : null}
                    {node.status === "revoked" ? (
                      <Button
                        variant="outline"
                        disabled={statusMutation.isPending}
                        onClick={() => statusMutation.mutate("active")}
                      >
                        <CirclePlay />
                        恢复授权
                      </Button>
                    ) : null}
                    <Separator className="my-1" />
                    <Button
                      variant="outline"
                      disabled={commandMutation.isPending}
                      onClick={() => commandMutation.mutate()}
                    >
                      {commandMutation.isPending ? (
                        <LoaderCircle className="animate-spin" />
                      ) : (
                        <KeyRound />
                      )}
                      生成部署命令
                    </Button>
                    <Button
                      disabled={!node.can_upgrade || upgrade.isPending}
                      title={node.upgrade_blocker}
                      onClick={() => upgrade.mutate()}
                    >
                      {upgrade.isPending ? (
                        <LoaderCircle className="animate-spin" />
                      ) : (
                        <Rocket />
                      )}
                      在线升级
                    </Button>
                    {node.upgrade_blocker ? (
                      <p className="text-xs leading-5 text-muted-foreground">
                        {node.upgrade_blocker}
                      </p>
                    ) : null}
                  </CardContent>
                </Card>
                <UninstallPanel
                  node={node}
                  status={uninstall.data}
                  pending={uninstallAction.isPending}
                  onAction={(path, method, body) =>
                    uninstallAction.mutate({ path, method, body })
                  }
                  onForce={() => setConfirm("force")}
                  onDelete={() => setConfirm("delete")}
                />
              </div>
            </section>
          </>
        ) : null}
      </PageBody>
      <CommandDialog
        command={command}
        onOpenChange={(open) => {
          if (!open) setCommand("");
        }}
      />
      <ConfirmDialog
        open={confirm === "force"}
        onOpenChange={(open) => {
          if (!open) setConfirm(null);
        }}
        title="强制完成卸载"
        description="仅在远端清理已人工核验完成时使用。控制面不会验证远端清理结果。"
        confirmation={node?.name}
        confirmLabel="强制完成"
        destructive
        busy={uninstallAction.isPending}
        onConfirm={async () => {
          await uninstallAction.mutateAsync({
            path: `/api/nodes/${encodeURIComponent(nodeId)}/uninstall/force-complete`,
            body: JSON.stringify({ confirmation: node?.name }),
          });
          setConfirm(null);
        }}
      />
      <ConfirmDialog
        open={confirm === "delete"}
        onOpenChange={(open) => {
          if (!open) setConfirm(null);
        }}
        title="删除节点记录"
        description="此操作会永久删除控制面中的节点记录，且无法撤销。"
        confirmation={node?.name}
        confirmLabel="永久删除"
        destructive
        busy={deleteNode.isPending}
        onConfirm={async () => {
          await deleteNode.mutateAsync();
          setConfirm(null);
        }}
      />
    </>
  );
}

function CacheQuotaSettings({
  nodeId,
  settings,
}: {
  nodeId: string;
  settings: NodeCacheSettings;
}) {
  const queryClient = useQueryClient();
  const [overrideEnabled, setOverrideEnabled] = useState(
    settings.override_size_gb != null,
  );
  const [size, setSize] = useState(
    settings.override_size_gb ?? settings.default_size_gb,
  );
  const mutation = useMutation({
    mutationFn: () =>
      api<NodeCacheSettings>(`/api/nodes/${encodeURIComponent(nodeId)}/cache`, {
        method: "PUT",
        body: JSON.stringify({
          cache_max_size_gb: overrideEnabled ? size : null,
        }),
      }),
    onSuccess: () => {
      toast.success("节点缓存配额已保存");
      void queryClient.invalidateQueries({ queryKey: ["node", nodeId] });
      void queryClient.invalidateQueries({ queryKey: ["nodes"] });
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const effectiveSize = overrideEnabled ? size : settings.default_size_gb;

  return (
    <Card>
      <CardHeader>
        <CardTitle>磁盘缓存配额</CardTitle>
        <CardDescription>当前配置 {effectiveSize} GB</CardDescription>
      </CardHeader>
      <CardContent>
        <form
          className="grid gap-4"
          onSubmit={(event) => {
            event.preventDefault();
            mutation.mutate();
          }}
        >
          <div className="flex items-center justify-between gap-4">
            <div>
              <Label htmlFor="override-node-cache">覆写全局缓存配额</Label>
              <p className="text-xs text-muted-foreground">
                全局默认 {settings.default_size_gb} GB
              </p>
            </div>
            <Switch
              id="override-node-cache"
              checked={overrideEnabled}
              onCheckedChange={setOverrideEnabled}
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="node-cache-size">节点缓存总上限（GB）</Label>
            <Input
              id="node-cache-size"
              type="number"
              min={1}
              max={1024}
              required={overrideEnabled}
              disabled={!overrideEnabled}
              value={size}
              onChange={(event) => setSize(Number(event.target.value))}
            />
          </div>
          <p className="text-xs text-muted-foreground">
            保存后立即下发到该节点。
          </p>
          <Button type="submit" disabled={mutation.isPending}>
            {mutation.isPending ? (
              <LoaderCircle className="animate-spin" />
            ) : (
              <Save />
            )}
            保存缓存配置
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}

function NginxCapacitySettings({
  nodeId,
  capacity,
  capacitySupported,
}: {
  nodeId: string;
  capacity: NginxCapacity;
  capacitySupported: boolean;
}) {
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState(capacity);
  const mutation = useMutation({
    mutationFn: () =>
      api<Node>(`/api/nodes/${encodeURIComponent(nodeId)}/nginx-capacity`, {
        method: "PUT",
        body: JSON.stringify(draft),
      }),
    onSuccess: () => {
      toast.success(
        capacitySupported
          ? "Nginx 容量配置已下发"
          : "Nginx 容量配置已保存，重新部署边缘节点后生效",
      );
      void queryClient.invalidateQueries({ queryKey: ["node", nodeId] });
      void queryClient.invalidateQueries({ queryKey: ["nodes"] });
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  return (
    <Card>
      <CardHeader>
        <CardTitle>Nginx 容量</CardTitle>
        <CardDescription>
          {capacitySupported
            ? "默认：自动进程、4096 连接、65536 文件句柄"
            : "当前节点完成容量配置升级后应用此配置"}
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form
          className="grid gap-4"
          onSubmit={(event) => {
            event.preventDefault();
            mutation.mutate();
          }}
        >
          <div className="grid gap-2">
            <Label htmlFor="nginx-worker-processes">工作进程（0 为自动）</Label>
            <Input
              id="nginx-worker-processes"
              type="number"
              min={0}
              max={128}
              required
              value={draft.worker_processes}
              onChange={(event) =>
                setDraft({
                  ...draft,
                  worker_processes: Number(event.target.value),
                })
              }
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="nginx-worker-connections">每进程连接数</Label>
            <Input
              id="nginx-worker-connections"
              type="number"
              min={256}
              max={65535}
              required
              value={draft.worker_connections}
              onChange={(event) =>
                setDraft({
                  ...draft,
                  worker_connections: Number(event.target.value),
                })
              }
            />
          </div>
          <div className="grid gap-2">
            <Label htmlFor="nginx-worker-nofile">每进程文件句柄</Label>
            <Input
              id="nginx-worker-nofile"
              type="number"
              min={1024}
              max={65536}
              required
              value={draft.worker_rlimit_nofile}
              onChange={(event) =>
                setDraft({
                  ...draft,
                  worker_rlimit_nofile: Number(event.target.value),
                })
              }
            />
          </div>
          <Button type="submit" disabled={mutation.isPending}>
            {mutation.isPending ? (
              <LoaderCircle className="animate-spin" />
            ) : (
              <Save />
            )}
            保存容量配置
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}

function NodeSummary({ detail }: { detail: NodeDetail }) {
  const node = detail.node;
  return (
    <Card>
      <CardHeader className="flex-row items-start justify-between">
        <div>
          <CardTitle>运行摘要</CardTitle>
          <CardDescription>代理与配置状态</CardDescription>
        </div>
        <StatusBadge status={node.status} />
      </CardHeader>
      <CardContent className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Datum
          label="最近心跳"
          value={formatDateTime(node.last_heartbeat_at)}
        />
        <Datum
          label="应用配置"
          value={`v${formatNumber(node.applied_version)}`}
        />
        <Datum
          label="代理版本"
          value={node.agent_version ? `v${node.agent_version}` : "--"}
        />
        <Datum
          label="目标版本"
          value={
            node.target_agent_version ? `v${node.target_agent_version}` : "--"
          }
        />
        <Datum label="代理摘要" value={shortHash(node.agent_sha256)} mono />
        <Datum label="关联站点" value={`${detail.sites.length} 个`} />
        {node.last_error ? (
          <Alert variant="destructive" className="sm:col-span-2 lg:col-span-4">
            <AlertTitle>节点报告错误</AlertTitle>
            <AlertDescription>{node.last_error}</AlertDescription>
          </Alert>
        ) : null}
      </CardContent>
    </Card>
  );
}

function MachineStatus({ detail }: { detail: NodeDetail }) {
  const machine = detail.machine;
  if (!machine.available || !machine.report)
    return (
      <Card>
        <CardHeader>
          <CardTitle>机器状态</CardTitle>
          <CardDescription>
            {machine.unavailable_reason || "暂无机器指标"}
          </CardDescription>
        </CardHeader>
      </Card>
    );
  const report = machine.report;
  const memory = report.memory_total_bytes
    ? (report.memory_used_bytes / report.memory_total_bytes) * 100
    : 0;
  const disk = report.disk_total_bytes
    ? (report.disk_used_bytes / report.disk_total_bytes) * 100
    : 0;
  return (
    <Card>
      <CardHeader className="flex-row items-start justify-between">
        <div>
          <CardTitle>机器状态</CardTitle>
          <CardDescription>
            {report.distribution} {report.version} · {report.cpu_logical_cores}{" "}
            核 · 运行 {formatDuration(report.uptime_seconds)}
          </CardDescription>
        </div>
        {machine.stale ? (
          <StatusBadge status="failed" label="数据过期" />
        ) : (
          <StatusBadge status="active" label="数据正常" />
        )}
      </CardHeader>
      <CardContent className="grid gap-5 sm:grid-cols-2 xl:grid-cols-3">
        <Usage
          label="CPU"
          value={report.cpu_usage_percent}
          detail={`负载 ${report.load_1.toFixed(2)} / ${report.load_5.toFixed(2)} / ${report.load_15.toFixed(2)}`}
        />
        <Usage
          label="内存"
          value={memory}
          detail={`${formatBytes(report.memory_used_bytes)} / ${formatBytes(report.memory_total_bytes)}`}
        />
        <Usage
          label="磁盘"
          value={disk}
          detail={`${formatBytes(report.disk_used_bytes)} / ${formatBytes(report.disk_total_bytes)}`}
        />
        <div className="sm:col-span-2 xl:col-span-3 flex flex-wrap gap-x-6 gap-y-2 border-t pt-4 text-sm">
          <span className="flex items-center gap-2">
            <Wifi className="size-4 text-muted-foreground" />
            {report.network_interface || "默认接口"}
          </span>
          <span>接收 {formatBytes(report.network_rx_bytes_per_second)}/s</span>
          <span>发送 {formatBytes(report.network_tx_bytes_per_second)}/s</span>
          <span className="ml-auto text-xs text-muted-foreground">
            采集于 {formatDateTime(report.collected_at)}
          </span>
        </div>
      </CardContent>
    </Card>
  );
}

function CacheStatus({
  query,
}: {
  query: ReturnType<typeof useQuery<NodeCacheStatus>>;
}) {
  const cache = query.data;
  if (query.error)
    return <PageError title="缓存状态加载失败" error={query.error} />;
  if (!cache)
    return (
      <Card>
        <CardHeader>
          <CardTitle>缓存状态</CardTitle>
          <CardDescription>正在加载</CardDescription>
        </CardHeader>
      </Card>
    );
  const storage = cache.storage;
  const used = storage.total_bytes
    ? (storage.used_bytes / storage.total_bytes) * 100
    : 0;
  return (
    <Card>
      <CardHeader>
        <CardTitle>缓存状态</CardTitle>
        <CardDescription>
          {cache.available
            ? `最近 24 小时 · 最后访问 ${formatDateTime(cache.last_seen_at)}`
            : cache.unavailable_reason}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-5">
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <Datum label="缓存命中率" value={formatPercent(cache.hit_rate, 2)} />
          <Datum label="缓存查询" value={formatNumber(cache.cache_lookups)} />
          <Datum label="绕过" value={formatNumber(cache.bypasses)} />
          <Datum label="未缓存" value={formatNumber(cache.uncached)} />
        </div>
        {storage.available ? (
          <div>
            <div className="mb-2 flex items-center justify-between text-sm">
              <span className="flex items-center gap-2">
                <HardDrive className="size-4 text-muted-foreground" />
                缓存空间
              </span>
              <span className="tabular-nums text-muted-foreground">
                {formatBytes(storage.used_bytes)} /{" "}
                {formatBytes(storage.total_bytes)}
              </span>
            </div>
            <Progress value={used} />
            <p className="mt-2 text-xs text-muted-foreground">
              {storage.stale ? "数据已过期 · " : ""}采集于{" "}
              {formatDateTime(storage.collected_at)}
            </p>
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">
            {storage.unavailable_reason}
          </p>
        )}
        {cache.statuses.length ? (
          <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-4">
            {cache.statuses.map((item) => (
              <div
                key={item.status}
                className="flex items-center justify-between rounded-lg border px-3 py-2 text-xs"
              >
                <StatusBadge status={item.status} />
                <span className="tabular-nums text-muted-foreground">
                  {formatNumber(item.requests)}
                </span>
              </div>
            ))}
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}

function AssignedSites({ sites }: { sites: NodeDetail["sites"] }) {
  const pagination = useListPagination(sites);

  return (
    <Card>
      <CardHeader>
        <CardTitle>关联站点</CardTitle>
        <CardDescription>当前草稿配置中的节点分配</CardDescription>
      </CardHeader>
      <CardContent className="px-0">
        {sites.length ? (
          <>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="pl-6">站点</TableHead>
                  <TableHead>状态</TableHead>
                  <TableHead>缓存</TableHead>
                  <TableHead className="pr-6 text-right">管理</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {pagination.items.map((site) => (
                  <TableRow key={site.id}>
                    <TableCell className="pl-6">
                      <div className="font-medium">{site.name}</div>
                      <div className="text-xs text-muted-foreground">
                        {site.domains.join(", ")}
                      </div>
                    </TableCell>
                    <TableCell>
                      <StatusBadge
                        status={site.published ? "succeeded" : "pending"}
                        label={site.published ? "已发布" : "未发布"}
                      />
                    </TableCell>
                    <TableCell>
                      {site.cache_enabled ? "启用" : "关闭"}
                    </TableCell>
                    <TableCell className="pr-6 text-right">
                      <Button asChild variant="outline" size="sm">
                        <Link to={`/sites/${encodeURIComponent(site.id)}`}>
                          查看
                        </Link>
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            <ListPagination pagination={pagination} itemLabel="个站点" />
          </>
        ) : (
          <div className="px-6 pb-6">
            <EmptyState title="未关联站点" />
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function UninstallPanel({
  node,
  status,
  pending,
  onAction,
  onForce,
  onDelete,
}: {
  node: Node;
  status?: NodeUninstallStatus;
  pending: boolean;
  onAction: (path: string, method?: string, body?: string) => void;
  onForce: () => void;
  onDelete: () => void;
}) {
  const base = `/api/nodes/${encodeURIComponent(node.id)}/uninstall`;
  const job = status?.job?.status === "canceled" ? undefined : status?.job;
  const blockersPagination = useListPagination(status?.blockers ?? []);
  const canPrepare = node.status === "draining" || node.status === "revoked";
  const deletable =
    node.status === "uninstalled" ||
    (node.status === "pending" &&
      !node.last_heartbeat_at &&
      !node.applied_version);
  return (
    <Card className="border-destructive/30">
      <CardHeader>
        <CardTitle>卸载与删除</CardTitle>
        <CardDescription>先从调度和 DNS 中移除，再执行远端卸载</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        {job ? (
          <div className="space-y-2 rounded-lg border bg-muted/30 p-3 text-sm">
            <div className="flex items-center justify-between">
              <span>卸载流程</span>
              <StatusBadge status={job.status} />
            </div>
            {status?.ready_in_seconds ? (
              <p className="text-xs text-muted-foreground">
                DNS 等待约 {status.ready_in_seconds} 秒
              </p>
            ) : null}
            {job.detail ? (
              <p className="text-xs text-muted-foreground">{job.detail}</p>
            ) : null}
            {blockersPagination.items.map((blocker) => (
              <p
                key={`${blocker.code}-${blocker.site_id}`}
                className="text-xs text-destructive"
              >
                {blocker.site_name
                  ? `${blocker.site_name}：${blocker.detail}`
                  : blocker.detail}
              </p>
            ))}
            {blockersPagination.totalPages > 1 ? (
              <ListPagination
                pagination={blockersPagination}
                itemLabel="项阻塞"
                className="mt-3 px-0"
              />
            ) : null}
          </div>
        ) : null}
        {!job ? (
          <Button
            variant="outline"
            disabled={!canPrepare || pending}
            onClick={() => onAction(base)}
          >
            <Unplug />
            准备卸载
          </Button>
        ) : null}
        {job && status?.can_generate_command ? (
          <Button
            variant="outline"
            disabled={pending}
            onClick={() => onAction(`${base}/command`)}
          >
            <Copy />
            生成卸载命令
          </Button>
        ) : null}
        {job && ["preparing", "ready", "failed"].includes(job.status) ? (
          <Button
            variant="outline"
            disabled={pending}
            onClick={() => onAction(base, "DELETE")}
          >
            <RefreshCw />
            取消卸载
          </Button>
        ) : null}
        {job &&
        status &&
        !status.blockers.length &&
        !status.ready_in_seconds &&
        ["preparing", "ready", "running", "failed"].includes(job.status) ? (
          <Button variant="destructive" disabled={pending} onClick={onForce}>
            <Unplug />
            强制完成
          </Button>
        ) : null}
        <Separator />
        <Button
          variant="destructive"
          disabled={!deletable || pending}
          onClick={onDelete}
        >
          <Trash2 />
          删除节点记录
        </Button>
        {!canPrepare && !job && !deletable ? (
          <p className="text-xs leading-5 text-muted-foreground">
            暂停调度或撤销授权后才能准备卸载。
          </p>
        ) : null}
      </CardContent>
    </Card>
  );
}

function CommandDialog({
  command,
  onOpenChange,
}: {
  command: string;
  onOpenChange: (open: boolean) => void;
}) {
  return (
    <Dialog open={Boolean(command)} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>节点命令</DialogTitle>
          <DialogDescription>
            在目标边缘服务器的 root shell 中执行
          </DialogDescription>
        </DialogHeader>
        <div className="relative">
          <pre className="max-h-72 overflow-auto whitespace-pre-wrap break-all rounded-lg border bg-terminal p-4 pr-12 text-xs leading-5 text-terminal-foreground">
            {command}
          </pre>
          <div className="absolute right-2 top-2">
            <CopyButton value={command} label="复制命令" />
          </div>
        </div>
        <DialogFooter>
          <Button onClick={() => onOpenChange(false)}>完成</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function Datum({
  label,
  value,
  mono = false,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div>
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className={`mt-1 text-sm font-medium ${mono ? "font-mono" : ""}`}>
        {value}
      </dd>
    </div>
  );
}
function Usage({
  label,
  value,
  detail,
}: {
  label: string;
  value: number;
  detail: string;
}) {
  return (
    <div>
      <div className="mb-2 flex items-center justify-between text-sm">
        <span>{label}</span>
        <span className="tabular-nums text-muted-foreground">
          {Math.max(0, value).toFixed(1)}%
        </span>
      </div>
      <Progress value={Math.max(0, Math.min(100, value))} />
      <p className="mt-2 text-xs text-muted-foreground">{detail}</p>
    </div>
  );
}
