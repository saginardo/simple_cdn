import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowRight,
  CheckCircle2,
  CirclePlus,
  Download,
  LoaderCircle,
  PackageCheck,
  RefreshCw,
  Rocket,
  Server,
  TriangleAlert,
} from "lucide-react";
import { useState, type FormEvent } from "react";
import { Link } from "react-router";
import { toast } from "sonner";
import { ConfirmDialog } from "@/components/confirm-dialog";
import {
  EmptyState,
  PageBody,
  PageError,
  PageHeader,
  PageLoading,
  Panel,
} from "@/components/page";
import { ListPagination } from "@/components/list-pagination";
import { StatusBadge } from "@/components/status-badge";
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
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { api, errorMessage } from "@/lib/api";
import {
  formatBytes,
  formatDateTime,
  formatNumber,
  shortHash,
} from "@/lib/format";
import type { NginxArtifactStatus, Node, NodeUpgradeTask } from "@/lib/types";
import { useListPagination } from "@/hooks/use-list-pagination";
import { t, useI18n } from "@/lib/i18n";
interface BulkUpgradeResult {
  created: number;
}
export function NodesPage() {
  useI18n();
  const queryClient = useQueryClient();
  const [createOpen, setCreateOpen] = useState(false);
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => api<Node[]>("/api/nodes"),
    refetchInterval: (query) =>
      query.state.data?.some((node) => activeUpgrade(node.upgrade_task))
        ? 5_000
        : 20_000,
  });
  const pagination = useListPagination(nodes.data ?? []);
  const bulkUpgrade = useMutation({
    mutationFn: () =>
      api<BulkUpgradeResult>("/api/nodes/upgrade-all", {
        method: "POST",
      }),
    onSuccess: (result) => {
      void queryClient.invalidateQueries({
        queryKey: ["nodes"],
      });
      toast.success(
        t("已创建 {value0} 个升级任务", {
          value0: result.created,
        }),
      );
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const upgradeable =
    nodes.data?.filter((node) => node.can_upgrade).length ?? 0;
  return (
    <>
      <PageHeader
        title={t("节点")}
        description={t("边缘节点、版本与在线运维")}
        actions={
          <>
            <Button
              variant="outline"
              disabled={!nodes.data?.length || bulkUpgrade.isPending}
              onClick={() => bulkUpgrade.mutate()}
            >
              {bulkUpgrade.isPending ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <Rocket />
              )}
              {t("全部升级")}
              {upgradeable ? ` (${upgradeable})` : ""}
            </Button>
            <Button onClick={() => setCreateOpen(true)}>
              <CirclePlus />
              {t("添加节点")}
            </Button>
          </>
        }
      />
      <PageBody>
        <NginxArtifactPanel />
        {nodes.isLoading ? <PageLoading /> : null}
        {nodes.error ? <PageError error={nodes.error} /> : null}
        {nodes.data ? (
          nodes.data.length ? (
            <Panel>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="pl-5">{t("节点")}</TableHead>
                    <TableHead>{t("状态")}</TableHead>
                    <TableHead>{t("公网地址")}</TableHead>
                    <TableHead>{t("心跳")}</TableHead>
                    <TableHead>{t("运行版本")}</TableHead>
                    <TableHead className="w-28">{t("升级")}</TableHead>
                    <TableHead className="w-12 pr-5">
                      <span className="sr-only">{t("管理")}</span>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {pagination.items.map((node) => (
                    <TableRow key={node.id}>
                      <TableCell className="pl-5">
                        <div className="font-medium">{node.name}</div>
                        <div className="font-mono text-xs text-muted-foreground">
                          {node.id}
                        </div>
                      </TableCell>
                      <TableCell>
                        <StatusBadge status={node.status} />
                      </TableCell>
                      <TableCell className="font-mono text-xs">
                        <div>{node.public_ipv4}</div>
                        <div className="mt-1 text-muted-foreground">
                          {node.public_ipv6 || t("IPv6 未配置")}
                        </div>
                      </TableCell>
                      <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                        {node.last_heartbeat_at
                          ? formatDateTime(node.last_heartbeat_at)
                          : t("尚未注册")}
                      </TableCell>
                      <TableCell>
                        <div className="text-sm font-medium">
                          {t("代理")}{" "}
                          {node.agent_version
                            ? `v${node.agent_version}`
                            : t("版本未知")}
                        </div>
                        <div className="text-xs text-muted-foreground">
                          Nginx{" "}
                          {node.nginx_version
                            ? `v${node.nginx_version}`
                            : t("版本未知")}{" "}
                          · {t("配置 v")}
                          {formatNumber(node.applied_version)}
                        </div>
                      </TableCell>
                      <TableCell>
                        <NodeUpgradeAction
                          node={node}
                          disabled={bulkUpgrade.isPending}
                        />
                      </TableCell>
                      <TableCell className="pr-5">
                        <Button asChild variant="ghost" size="icon-sm">
                          <Link
                            to={`/nodes/${encodeURIComponent(node.id)}`}
                            aria-label={t("管理 {value0}", {
                              value0: node.name,
                            })}
                          >
                            <ArrowRight />
                          </Link>
                        </Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
              <ListPagination
                pagination={pagination}
                itemLabel={t("个节点")}
                action={
                  <Button
                    variant="ghost"
                    size="icon-xs"
                    aria-label={t("刷新节点")}
                    onClick={() => void nodes.refetch()}
                  >
                    <RefreshCw
                      className={nodes.isFetching ? "animate-spin" : undefined}
                    />
                  </Button>
                }
              />
            </Panel>
          ) : (
            <EmptyState
              title={t("暂无边缘节点")}
              description={t("添加节点后生成安全的部署命令")}
            />
          )
        ) : null}
      </PageBody>
      <CreateNodeDialog open={createOpen} onOpenChange={setCreateOpen} />
    </>
  );
}

function NginxArtifactPanel() {
  const queryClient = useQueryClient();
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [approval, setApproval] = useState<{
    sha256: string;
    version: string;
  }>();
  const status = useQuery({
    queryKey: ["nginx-artifacts"],
    queryFn: () => api<NginxArtifactStatus>("/api/nginx/artifacts"),
    refetchInterval: (query) => (query.state.data?.checking ? 2_000 : 60_000),
  });
  const check = useMutation({
    mutationFn: () =>
      api<NginxArtifactStatus>("/api/nginx/artifacts/check", {
        method: "POST",
      }),
    onSuccess: (result) => {
      queryClient.setQueryData(["nginx-artifacts"], result);
      void queryClient.invalidateQueries({ queryKey: ["messages"] });
      toast.success(
        result.candidate
          ? t("Nginx 候选版本已就绪")
          : t("未发现新的 Nginx 稳定版"),
      );
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const promote = useMutation({
    mutationFn: (sha256: string) =>
      api<NginxArtifactStatus>(
        `/api/nginx/artifacts/${encodeURIComponent(sha256)}/promote`,
        { method: "POST" },
      ),
    onSuccess: (result) => {
      queryClient.setQueryData(["nginx-artifacts"], result);
      void queryClient.invalidateQueries({ queryKey: ["nodes"] });
      void queryClient.invalidateQueries({ queryKey: ["messages"] });
      setConfirmOpen(false);
      toast.success(t("Nginx 升级目标已更新"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  if (status.error) {
    return (
      <PageError title={t("Nginx 版本状态加载失败")} error={status.error} />
    );
  }
  if (!status.data) return null;
  const data = status.data;
  const candidate = data.candidate;
  return (
    <>
      <Panel>
        <div className="flex flex-col gap-3 border-b px-4 py-3 sm:flex-row sm:items-center sm:justify-between">
          <div className="flex min-w-0 items-center gap-2">
            <PackageCheck className="size-4 shrink-0 text-primary" />
            <div className="min-w-0">
              <div className="font-medium">{t("受管 Nginx")}</div>
              <div className="truncate text-xs text-muted-foreground">
                {data.repository || t("自动检查未配置")}
              </div>
            </div>
          </div>
          <Button
            variant="outline"
            size="sm"
            disabled={!data.enabled || check.isPending || data.checking}
            onClick={() => check.mutate()}
          >
            {check.isPending || data.checking ? (
              <LoaderCircle className="animate-spin" />
            ) : (
              <RefreshCw />
            )}
            {t("检查更新")}
          </Button>
        </div>
        <div className="grid md:grid-cols-2">
          <div className="px-4 py-3 md:border-r">
            <div className="text-xs text-muted-foreground">
              {t("当前升级目标")}
            </div>
            <div className="mt-1 flex items-center gap-2">
              <CheckCircle2 className="size-4 text-success" />
              <span className="font-medium">Nginx v{data.current.version}</span>
              <StatusBadge
                status="succeeded"
                label={data.current.managed ? t("已批准") : t("内置兜底")}
              />
            </div>
            <div
              className="mt-1 font-mono text-xs text-muted-foreground"
              title={data.current.sha256}
            >
              {shortHash(data.current.sha256)}
            </div>
          </div>
          <div className="border-t px-4 py-3 md:border-t-0">
            <div className="text-xs text-muted-foreground">
              {t("待批准候选")}
            </div>
            {candidate ? (
              <div className="mt-1 flex flex-wrap items-center justify-between gap-3">
                <div className="min-w-0">
                  <div className="flex items-center gap-2 font-medium">
                    <Download className="size-4 text-info" />
                    Nginx v{candidate.version}
                  </div>
                  <div className="mt-1 text-xs text-muted-foreground">
                    {formatBytes(candidate.size_bytes)} ·{" "}
                    {formatDateTime(candidate.downloaded_at)}
                  </div>
                </div>
                <Button
                  size="sm"
                  onClick={() => {
                    setApproval({
                      sha256: candidate.sha256,
                      version: candidate.version,
                    });
                    setConfirmOpen(true);
                  }}
                >
                  <CheckCircle2 />
                  {t("设为升级目标")}
                </Button>
              </div>
            ) : (
              <div className="mt-1 text-sm text-muted-foreground">
                {t("暂无待批准版本")}
              </div>
            )}
          </div>
        </div>
        {data.artifact_error || data.last_error ? (
          <div className="flex items-start gap-2 border-t px-4 py-2 text-xs text-destructive">
            <TriangleAlert className="mt-0.5 size-3.5 shrink-0" />
            <span>{data.artifact_error || data.last_error}</span>
          </div>
        ) : data.last_checked_at ? (
          <div className="border-t px-4 py-2 text-xs text-muted-foreground">
            {t("上次检查")} {formatDateTime(data.last_checked_at)}
          </div>
        ) : null}
      </Panel>
      <ConfirmDialog
        open={confirmOpen}
        onOpenChange={setConfirmOpen}
        title={t("批准 Nginx {version}", { version: approval?.version ?? "" })}
        description={t(
          "该版本将成为新建和在线升级任务的 Nginx 目标，现有节点不会自动升级。",
        )}
        confirmLabel={t("设为升级目标")}
        busy={promote.isPending}
        onConfirm={() => approval && promote.mutate(approval.sha256)}
      />
    </>
  );
}

function NodeUpgradeAction({
  node,
  disabled,
}: {
  node: Node;
  disabled: boolean;
}) {
  const queryClient = useQueryClient();
  const upgrade = useMutation({
    mutationFn: () =>
      api<Node>(`/api/nodes/${encodeURIComponent(node.id)}/upgrade`, {
        method: "POST",
      }),
    onSuccess: (updatedNode) => {
      queryClient.setQueryData<Node[]>(["nodes"], (current) =>
        current?.map((item) =>
          item.id === updatedNode.id ? updatedNode : item,
        ),
      );
      toast.success(
        t("节点 {value0} 升级已启动", {
          value0: node.name,
        }),
      );
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const task = node.upgrade_task;

  if (upgrade.isPending) {
    return (
      <Button
        variant="outline"
        size="xs"
        disabled
        className="min-w-[4.75rem] border-info/25 bg-info/10 text-info disabled:opacity-100"
        aria-label={`${node.name} · ${t("正在启动")}`}
      >
        <LoaderCircle className="animate-spin" />
        {t("正在启动")}
      </Button>
    );
  }

  if (task && activeUpgrade(task)) {
    const label = task.status === "queued" ? t("排队中") : t("升级中");
    return (
      <Button
        variant="outline"
        size="xs"
        disabled
        title={task.detail}
        className={
          task.status === "queued"
            ? "min-w-[4.75rem] border-warning/25 bg-warning/10 text-warning disabled:opacity-100"
            : "min-w-[4.75rem] border-info/25 bg-info/10 text-info disabled:opacity-100"
        }
        aria-label={`${node.name} · ${label}`}
      >
        <LoaderCircle className="animate-spin" />
        {label}
      </Button>
    );
  }

  if (node.upgrade_up_to_date) {
    return <StatusBadge status="succeeded" label={t("最新")} />;
  }

  if (node.can_upgrade) {
    const label = t("升级节点 {value0}", { value0: node.name });
    return (
      <Button
        variant="outline"
        size="xs"
        disabled={disabled}
        title={label}
        aria-label={label}
        onClick={() => upgrade.mutate()}
        className="min-w-[4.75rem] border-info/30 bg-info/5 text-info shadow-none hover:border-info/50 hover:bg-info/10 hover:text-info"
      >
        <Rocket />
        {t("可升级")}
      </Button>
    );
  }

  return (
    <span
      className="block max-w-32 whitespace-normal text-xs leading-4 text-muted-foreground"
      title={node.upgrade_blocker}
    >
      {node.upgrade_blocker || t("不可升级")}
    </span>
  );
}

function CreateNodeDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [ip, setIP] = useState("");
  const [ipv6, setIPv6] = useState("");
  const mutation = useMutation({
    mutationFn: () =>
      api<Node>("/api/nodes", {
        method: "POST",
        body: JSON.stringify({
          name,
          public_ipv4: ip,
          public_ipv6: ipv6,
        }),
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["nodes"],
      });
      toast.success(t("节点已添加"));
      setName("");
      setIP("");
      setIPv6("");
      onOpenChange(false);
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
            <DialogTitle>{t("添加边缘节点")}</DialogTitle>
            <DialogDescription>
              {t("创建节点记录后，在详情页生成部署命令")}
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 py-5">
            <div className="grid gap-2">
              <Label htmlFor="node-name">{t("节点名称")}</Label>
              <Input
                id="node-name"
                required
                maxLength={80}
                value={name}
                onChange={(event) => setName(event.target.value)}
                placeholder="edge-shanghai-01"
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="node-ip">{t("公网 IPv4")}</Label>
              <Input
                id="node-ip"
                required
                value={ip}
                onChange={(event) => setIP(event.target.value)}
                placeholder="203.0.113.10"
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="node-ipv6">{t("公网 IPv6（可选）")}</Label>
              <Input
                id="node-ipv6"
                value={ipv6}
                onChange={(event) => setIPv6(event.target.value)}
                placeholder="2001:db8::10"
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
                <Server />
              )}
              {t("创建节点")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
function activeUpgrade(task?: NodeUpgradeTask) {
  return task?.status === "queued" || task?.status === "applying";
}
