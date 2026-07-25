import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowRight,
  CirclePlus,
  LoaderCircle,
  RefreshCw,
  Rocket,
  Server,
} from "lucide-react";
import { useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { toast } from "sonner";
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
import { formatDateTime, formatNumber } from "@/lib/format";
import type { Node, NodeUpgradeTask } from "@/lib/types";
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
                    <TableHead>{t("公网 IPv4")}</TableHead>
                    <TableHead>{t("心跳")}</TableHead>
                    <TableHead>{t("代理版本")}</TableHead>
                    <TableHead>{t("升级")}</TableHead>
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
                        {node.public_ipv4}
                      </TableCell>
                      <TableCell className="whitespace-nowrap text-xs text-muted-foreground">
                        {node.last_heartbeat_at
                          ? formatDateTime(node.last_heartbeat_at)
                          : t("尚未注册")}
                      </TableCell>
                      <TableCell>
                        <div className="text-sm font-medium">
                          {node.agent_version
                            ? `v${node.agent_version}`
                            : t("版本未知")}
                        </div>
                        <div className="text-xs text-muted-foreground">
                          {t("配置 v")}
                          {formatNumber(node.applied_version)}
                        </div>
                      </TableCell>
                      <TableCell>
                        {node.upgrade_task &&
                        activeUpgrade(node.upgrade_task) ? (
                          <StatusBadge status={node.upgrade_task.status} />
                        ) : node.upgrade_up_to_date ? (
                          <StatusBadge status="succeeded" label={t("最新")} />
                        ) : node.can_upgrade ? (
                          <StatusBadge status="ready" label={t("可升级")} />
                        ) : (
                          <span className="text-xs text-muted-foreground">
                            {node.upgrade_blocker || t("不可升级")}
                          </span>
                        )}
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
  const mutation = useMutation({
    mutationFn: () =>
      api<Node>("/api/nodes", {
        method: "POST",
        body: JSON.stringify({
          name,
          public_ipv4: ip,
        }),
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({
        queryKey: ["nodes"],
      });
      toast.success(t("节点已添加"));
      setName("");
      setIP("");
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
