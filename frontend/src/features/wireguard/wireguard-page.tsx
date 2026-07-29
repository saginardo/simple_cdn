import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Activity,
  Cable,
  CirclePlus,
  Eye,
  Gauge,
  LoaderCircle,
  Pencil,
  RefreshCw,
  Server,
  Terminal,
  Trash2,
  Unplug,
} from "lucide-react";
import {
  useEffect,
  useMemo,
  useState,
  type FormEvent,
  type ReactNode,
} from "react";
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
  Panel,
} from "@/components/page";
import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
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
import {
  formatBytes,
  formatDateTime,
  formatNumber,
  formatPercent,
  shortHash,
} from "@/lib/format";
import { t, useI18n } from "@/lib/i18n";
import type {
  Node,
  WireGuardPeer,
  WireGuardPerformanceTest,
  WireGuardTCPMeasurement,
  WireGuardTunnel,
} from "@/lib/types";

interface TunnelDraft {
  name: string;
  endpoint_host: string;
  listen_port: number;
  address_cidr: string;
  mtu: number;
  persistent_keepalive_seconds: number;
  performance_port: number;
  origin_egress_limit_mbps: number;
  node_ids: string[];
  edge_egress_limits_mbps: Record<string, number>;
}

interface CommandState {
  title: string;
  command: string;
  expiresAt?: string;
}

export function WireGuardPage() {
  useI18n();
  const queryClient = useQueryClient();
  const [section, setSection] = useState("tunnels");
  const [editTunnel, setEditTunnel] = useState<
    WireGuardTunnel | null | undefined
  >();
  const [detailTunnel, setDetailTunnel] = useState<WireGuardTunnel | null>(
    null,
  );
  const [deleteTunnel, setDeleteTunnel] = useState<WireGuardTunnel | null>(
    null,
  );
  const [performanceTunnel, setPerformanceTunnel] = useState<
    WireGuardTunnel | null | undefined
  >();
  const [command, setCommand] = useState<CommandState | null>(null);

  const tunnels = useQuery({
    queryKey: ["wireguard-tunnels"],
    queryFn: () => api<WireGuardTunnel[]>("/api/wireguard/tunnels"),
    refetchInterval: 10_000,
  });
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => api<Node[]>("/api/nodes"),
  });
  const tests = useQuery({
    queryKey: ["wireguard-performance-tests"],
    queryFn: () =>
      api<WireGuardPerformanceTest[]>(
        "/api/wireguard/performance-tests?limit=100",
      ),
    refetchInterval: (query) =>
      query.state.data?.some((test) =>
        ["queued", "running"].includes(test.status),
      )
        ? 2_000
        : 10_000,
  });

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ["wireguard-tunnels"] });
    void queryClient.invalidateQueries({
      queryKey: ["wireguard-performance-tests"],
    });
    void queryClient.invalidateQueries({ queryKey: ["nodes"] });
  };

  const remove = useMutation({
    mutationFn: (tunnel: WireGuardTunnel) =>
      api<{ ok: boolean }>(
        `/api/wireguard/tunnels/${encodeURIComponent(tunnel.id)}`,
        {
          method: "DELETE",
          ...jsonBody({ confirmation: tunnel.name }),
        },
      ),
    onSuccess: () => {
      setDeleteTunnel(null);
      refresh();
      toast.success(t("WireGuard 隧道已删除"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  const install = useMutation({
    mutationFn: (tunnel: WireGuardTunnel) =>
      api<{ install_command: string; expires_at: string }>(
        `/api/wireguard/tunnels/${encodeURIComponent(tunnel.id)}/install-command`,
        { method: "POST" },
      ),
    onSuccess: (result, tunnel) =>
      setCommand({
        title: t("{value0} 源站安装命令", { value0: tunnel.name }),
        command: result.install_command,
        expiresAt: result.expires_at,
      }),
    onError: (error) => toast.error(errorMessage(error)),
  });

  const uninstall = useMutation({
    mutationFn: (tunnel: WireGuardTunnel) =>
      api<{ uninstall_command: string }>(
        `/api/wireguard/tunnels/${encodeURIComponent(tunnel.id)}/uninstall-command`,
      ),
    onSuccess: (result, tunnel) =>
      setCommand({
        title: t("{value0} 源站卸载命令", { value0: tunnel.name }),
        command: result.uninstall_command,
      }),
    onError: (error) => toast.error(errorMessage(error)),
  });

  const data = tunnels.data ?? [];
  const peers = data.flatMap((tunnel) => tunnel.peers);
  const readyTunnels = data.filter(tunnelPerformanceReady);
  const freshHandshakes = peers.filter((peer) =>
    handshakeFresh(peer.latest_handshake_at),
  );
  const tunnelPagination = useListPagination(data);
  const testPagination = useListPagination(tests.data ?? []);

  return (
    <>
      <PageHeader
        title="WireGuard"
        description={t("源站隧道、节点状态与链路性能")}
        actions={
          <>
            <Button
              variant="outline"
              size="icon"
              aria-label={t("刷新 WireGuard 数据")}
              onClick={refresh}
            >
              <RefreshCw
                className={
                  tunnels.isFetching || tests.isFetching ? "animate-spin" : ""
                }
              />
            </Button>
            <Button
              variant="outline"
              onClick={() => setPerformanceTunnel(null)}
              disabled={!readyTunnels.length}
            >
              <Gauge />
              {t("链路测试")}
            </Button>
            <Button onClick={() => setEditTunnel(null)}>
              <CirclePlus />
              {t("添加隧道")}
            </Button>
          </>
        }
      />
      <PageBody>
        {tunnels.isLoading || nodes.isLoading || tests.isLoading ? (
          <PageLoading />
        ) : null}
        {tunnels.error || nodes.error || tests.error ? (
          <PageError error={tunnels.error || nodes.error || tests.error} />
        ) : null}
        {tunnels.data && nodes.data && tests.data ? (
          <>
            <Panel>
              <div className="grid divide-y sm:grid-cols-2 sm:divide-x sm:divide-y-0 xl:grid-cols-4">
                <Summary
                  icon={<Cable />}
                  label={t("可发布隧道")}
                  value={`${readyTunnels.length} / ${data.length}`}
                  detail={t("源站与边缘修订一致")}
                />
                <Summary
                  icon={<Server />}
                  label={t("边缘 Peer")}
                  value={formatNumber(peers.length)}
                  detail={t("{value0} 个已应用", {
                    value0: data.reduce(
                      (count, tunnel) =>
                        count +
                        tunnel.peers.filter((peer) => peerApplied(peer, tunnel))
                          .length,
                      0,
                    ),
                  })}
                />
                <Summary
                  icon={<Activity />}
                  label={t("近期握手")}
                  value={`${freshHandshakes.length} / ${peers.length}`}
                  detail={t("最近 3 分钟")}
                />
                <Summary
                  icon={<Gauge />}
                  label={t("性能任务")}
                  value={formatNumber(tests.data.length)}
                  detail={t("含公网 TCP 对照")}
                />
              </div>
            </Panel>

            <Tabs
              value={section}
              onValueChange={setSection}
              className="space-y-4"
            >
              <TabsList>
                <TabsTrigger value="tunnels">{t("隧道")}</TabsTrigger>
                <TabsTrigger value="performance">{t("性能测试")}</TabsTrigger>
              </TabsList>
              <TabsContent value="tunnels">
                {data.length ? (
                  <Panel>
                    <Table className="min-w-[1040px]">
                      <TableHeader>
                        <TableRow>
                          <TableHead className="pl-5">{t("名称")}</TableHead>
                          <TableHead>{t("公网端点")}</TableHead>
                          <TableHead>{t("隧道地址")}</TableHead>
                          <TableHead>{t("源站修订")}</TableHead>
                          <TableHead>{t("边缘状态")}</TableHead>
                          <TableHead>{t("最近握手")}</TableHead>
                          <TableHead className="w-44 pr-5 text-right">
                            {t("操作")}
                          </TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {tunnelPagination.items.map((tunnel) => (
                          <TableRow key={tunnel.id}>
                            <TableCell className="pl-5">
                              <div className="font-medium">{tunnel.name}</div>
                              <code className="text-xs text-muted-foreground">
                                {tunnel.id}
                              </code>
                            </TableCell>
                            <TableCell>
                              <code>
                                {tunnel.endpoint_host}:{tunnel.listen_port}
                              </code>
                            </TableCell>
                            <TableCell>
                              <div className="font-mono text-xs">
                                {tunnel.origin_address}
                              </div>
                              <div className="text-xs text-muted-foreground">
                                {tunnel.address_cidr}
                              </div>
                            </TableCell>
                            <TableCell>
                              <StatusBadge
                                status={
                                  originReady(tunnel) ? "ready" : "pending"
                                }
                                label={
                                  originReady(tunnel)
                                    ? t("r{value0} 已应用", {
                                        value0: tunnel.revision,
                                      })
                                    : t("r{value0} / r{value1}", {
                                        value0:
                                          tunnel.origin_configured_revision,
                                        value1: tunnel.revision,
                                      })
                                }
                              />
                            </TableCell>
                            <TableCell>
                              <StatusBadge
                                status={
                                  tunnelReady(tunnel) ? "ready" : "pending"
                                }
                                label={t("{value0}/{value1} 已应用", {
                                  value0: tunnel.peers.filter((peer) =>
                                    peerApplied(peer, tunnel),
                                  ).length,
                                  value1: tunnel.peers.length,
                                })}
                              />
                            </TableCell>
                            <TableCell>
                              {formatDateTime(latestHandshake(tunnel))}
                            </TableCell>
                            <TableCell className="pr-5">
                              <div className="flex justify-end gap-1">
                                <IconAction
                                  label={t("查看隧道")}
                                  onClick={() => setDetailTunnel(tunnel)}
                                >
                                  <Eye />
                                </IconAction>
                                <IconAction
                                  label={t("生成源站安装命令")}
                                  onClick={() => install.mutate(tunnel)}
                                  disabled={install.isPending}
                                >
                                  <Terminal />
                                </IconAction>
                                <IconAction
                                  label={t("测试隧道")}
                                  onClick={() => setPerformanceTunnel(tunnel)}
                                  disabled={!tunnelPerformanceReady(tunnel)}
                                >
                                  <Gauge />
                                </IconAction>
                                <IconAction
                                  label={t("编辑隧道")}
                                  onClick={() => setEditTunnel(tunnel)}
                                >
                                  <Pencil />
                                </IconAction>
                                <IconAction
                                  label={t("删除隧道")}
                                  onClick={() => setDeleteTunnel(tunnel)}
                                >
                                  <Trash2 />
                                </IconAction>
                              </div>
                            </TableCell>
                          </TableRow>
                        ))}
                      </TableBody>
                    </Table>
                    <ListPagination
                      pagination={tunnelPagination}
                      itemLabel={t("条隧道")}
                    />
                  </Panel>
                ) : (
                  <EmptyState title={t("暂无 WireGuard 隧道")} />
                )}
              </TabsContent>
              <TabsContent value="performance">
                {tests.data.length ? (
                  <PerformanceTable tests={testPagination.items} />
                ) : (
                  <EmptyState title={t("暂无性能测试")} />
                )}
                {tests.data.length ? (
                  <ListPagination
                    pagination={testPagination}
                    itemLabel={t("条测试")}
                    className="rounded-lg border border-t-0"
                  />
                ) : null}
              </TabsContent>
            </Tabs>
          </>
        ) : null}
      </PageBody>

      <TunnelDialog
        open={editTunnel !== undefined}
        tunnel={editTunnel ?? null}
        nodes={nodes.data ?? []}
        onOpenChange={(open) => {
          if (!open) setEditTunnel(undefined);
        }}
        onSaved={refresh}
      />
      <PerformanceDialog
        open={performanceTunnel !== undefined}
        initialTunnel={performanceTunnel ?? null}
        tunnels={data}
        onOpenChange={(open) => {
          if (!open) setPerformanceTunnel(undefined);
        }}
        onStarted={() => {
          setPerformanceTunnel(undefined);
          setSection("performance");
          refresh();
        }}
      />
      <TunnelDetailDialog
        tunnel={detailTunnel}
        onOpenChange={(open) => {
          if (!open) setDetailTunnel(null);
        }}
        onUninstall={(tunnel) => {
          setDetailTunnel(null);
          uninstall.mutate(tunnel);
        }}
      />
      <CommandDialog
        command={command}
        onOpenChange={(open) => !open && setCommand(null)}
      />
      <ConfirmDialog
        open={Boolean(deleteTunnel)}
        onOpenChange={(open) => !open && setDeleteTunnel(null)}
        title={t("删除 WireGuard 隧道")}
        description={t("已被站点引用的隧道不能删除。")}
        confirmation={deleteTunnel?.name}
        confirmLabel={t("删除隧道")}
        destructive
        busy={remove.isPending}
        onConfirm={async () => {
          if (deleteTunnel) await remove.mutateAsync(deleteTunnel);
        }}
      />
    </>
  );
}

function TunnelDialog({
  open,
  tunnel,
  nodes,
  onOpenChange,
  onSaved,
}: {
  open: boolean;
  tunnel: WireGuardTunnel | null;
  nodes: Node[];
  onOpenChange: (open: boolean) => void;
  onSaved: () => void;
}) {
  const [draft, setDraft] = useState<TunnelDraft>(() => tunnelDraft(tunnel));
  const suggested = useQuery({
    queryKey: ["wireguard-suggested-cidr"],
    queryFn: () =>
      api<{ address_cidr: string }>("/api/wireguard/suggested-cidr"),
    enabled: open && !tunnel,
    staleTime: 0,
  });
  useEffect(() => {
    if (open) setDraft(tunnelDraft(tunnel));
  }, [open, tunnel]);
  useEffect(() => {
    if (
      open &&
      !tunnel &&
      suggested.data?.address_cidr &&
      !draft.address_cidr
    ) {
      setDraft((current) => ({
        ...current,
        address_cidr: suggested.data!.address_cidr,
      }));
    }
  }, [draft.address_cidr, open, suggested.data, tunnel]);

  const save = useMutation({
    mutationFn: () =>
      api<WireGuardTunnel>(
        tunnel
          ? `/api/wireguard/tunnels/${encodeURIComponent(tunnel.id)}`
          : "/api/wireguard/tunnels",
        {
          method: tunnel ? "PUT" : "POST",
          ...jsonBody(draft),
        },
      ),
    onSuccess: () => {
      onOpenChange(false);
      onSaved();
      toast.success(
        t(tunnel ? "WireGuard 隧道已更新" : "WireGuard 隧道已创建"),
      );
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  const availableNodes = nodes.filter(
    (node) =>
      draft.node_ids.includes(node.id) ||
      (!["revoked", "uninstalling", "uninstalled"].includes(node.status) &&
        node.capabilities?.includes("wireguard_v1")),
  );
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!draft.node_ids.length) {
      toast.error(t("至少选择一个支持 WireGuard 的边缘节点"));
      return;
    }
    save.mutate();
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-3xl">
        <form onSubmit={submit} className="space-y-5">
          <DialogHeader>
            <DialogTitle>
              {t(tunnel ? "编辑 WireGuard 隧道" : "添加 WireGuard 隧道")}
            </DialogTitle>
            <DialogDescription>
              {t("隧道参数与边缘 Peer 分配")}
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label={t("名称")} id="wireguard-name">
              <Input
                id="wireguard-name"
                required
                maxLength={100}
                value={draft.name}
                onChange={(event) =>
                  setDraft({ ...draft, name: event.target.value })
                }
              />
            </Field>
            <Field label={t("源站公网地址")} id="wireguard-endpoint">
              <Input
                id="wireguard-endpoint"
                required
                value={draft.endpoint_host}
                onChange={(event) =>
                  setDraft({ ...draft, endpoint_host: event.target.value })
                }
                placeholder="origin.example.com"
              />
            </Field>
            <Field label={t("WireGuard UDP 端口")} id="wireguard-port">
              <Input
                id="wireguard-port"
                required
                type="number"
                min={1}
                max={65535}
                value={draft.listen_port}
                onChange={(event) =>
                  setDraft({
                    ...draft,
                    listen_port: Number(event.target.value),
                  })
                }
              />
            </Field>
            <Field label={t("隧道 CIDR")} id="wireguard-cidr">
              <Input
                id="wireguard-cidr"
                required
                value={draft.address_cidr}
                onChange={(event) =>
                  setDraft({ ...draft, address_cidr: event.target.value })
                }
                placeholder="10.253.0.0/24"
              />
            </Field>
            <Field label="MTU" id="wireguard-mtu">
              <Input
                id="wireguard-mtu"
                required
                type="number"
                min={1280}
                max={1500}
                value={draft.mtu}
                onChange={(event) =>
                  setDraft({ ...draft, mtu: Number(event.target.value) })
                }
              />
            </Field>
            <Field label={t("保活间隔（秒）")} id="wireguard-keepalive">
              <Input
                id="wireguard-keepalive"
                required
                type="number"
                min={1}
                max={120}
                value={draft.persistent_keepalive_seconds}
                onChange={(event) =>
                  setDraft({
                    ...draft,
                    persistent_keepalive_seconds: Number(event.target.value),
                  })
                }
              />
            </Field>
            <Field label={t("性能测试端口")} id="wireguard-performance-port">
              <Input
                id="wireguard-performance-port"
                required
                type="number"
                min={1}
                max={65535}
                value={draft.performance_port}
                onChange={(event) =>
                  setDraft({
                    ...draft,
                    performance_port: Number(event.target.value),
                  })
                }
              />
            </Field>
            <Field
              label={t("源站出口上限（Mbps）")}
              id="wireguard-origin-egress-limit"
            >
              <Input
                id="wireguard-origin-egress-limit"
                required
                type="number"
                min={0}
                max={10000}
                value={draft.origin_egress_limit_mbps}
                onChange={(event) =>
                  setDraft({
                    ...draft,
                    origin_egress_limit_mbps: Number(event.target.value),
                  })
                }
              />
            </Field>
          </div>
          <div className="grid gap-2">
            <Label>{t("边缘节点")}</Label>
            {availableNodes.length ? (
              <div className="grid gap-2 sm:grid-cols-2">
                {availableNodes.map((node) => {
                  const checked = draft.node_ids.includes(node.id);
                  const sameHost =
                    draft.endpoint_host.trim().toLowerCase() ===
                    node.public_ipv4;
                  const limitID = `wireguard-edge-limit-${node.id}`;
                  return (
                    <div
                      key={node.id}
                      className="grid min-w-0 gap-3 rounded-md border px-3 py-3"
                    >
                      <div className="flex min-w-0 items-center gap-3">
                        <Checkbox
                          id={`wireguard-node-${node.id}`}
                          checked={checked}
                          disabled={sameHost && !checked}
                          onCheckedChange={(value) =>
                            setDraft((current) => {
                              const limits = {
                                ...current.edge_egress_limits_mbps,
                              };
                              if (value) {
                                limits[node.id] ??= 0;
                              } else {
                                delete limits[node.id];
                              }
                              return {
                                ...current,
                                node_ids: value
                                  ? current.node_ids.includes(node.id)
                                    ? current.node_ids
                                    : [...current.node_ids, node.id]
                                  : current.node_ids.filter(
                                      (id) => id !== node.id,
                                    ),
                                edge_egress_limits_mbps: limits,
                              };
                            })
                          }
                        />
                        <Label
                          htmlFor={`wireguard-node-${node.id}`}
                          className="min-w-0 flex-1 cursor-pointer"
                        >
                          <span className="block truncate text-sm font-medium">
                            {node.name}
                          </span>
                          <span className="block font-mono text-xs font-normal text-muted-foreground">
                            {node.public_ipv4}
                          </span>
                        </Label>
                      </div>
                      <div className="grid gap-1.5">
                        <Label htmlFor={limitID} className="text-xs">
                          {t("边缘出口上限（Mbps）")}
                        </Label>
                        <Input
                          id={limitID}
                          type="number"
                          min={0}
                          max={10000}
                          disabled={!checked}
                          value={draft.edge_egress_limits_mbps[node.id] ?? 0}
                          onChange={(event) =>
                            setDraft((current) => ({
                              ...current,
                              edge_egress_limits_mbps: {
                                ...current.edge_egress_limits_mbps,
                                [node.id]: Number(event.target.value),
                              },
                            }))
                          }
                        />
                      </div>
                      {sameHost ? (
                        <p className="text-xs text-destructive">
                          {t("与源站公网地址相同，不能选择")}
                        </p>
                      ) : null}
                    </div>
                  );
                })}
              </div>
            ) : (
              <div className="rounded-lg border border-dashed px-4 py-6 text-center text-sm text-muted-foreground">
                {t("暂无支持 WireGuard 的边缘节点")}
              </div>
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
              disabled={save.isPending || !availableNodes.length}
            >
              {save.isPending ? (
                <LoaderCircle className="animate-spin" />
              ) : null}
              {t("保存")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function PerformanceDialog({
  open,
  initialTunnel,
  tunnels,
  onOpenChange,
  onStarted,
}: {
  open: boolean;
  initialTunnel: WireGuardTunnel | null;
  tunnels: WireGuardTunnel[];
  onOpenChange: (open: boolean) => void;
  onStarted: () => void;
}) {
  const ready = useMemo(
    () => tunnels.filter(tunnelPerformanceReady),
    [tunnels],
  );
  const [tunnelID, setTunnelID] = useState("");
  const [nodeID, setNodeID] = useState("");
  const [targetMbps, setTargetMbps] = useState(100);
  const [durationSeconds, setDurationSeconds] = useState(10);
  const selected = ready.find((tunnel) => tunnel.id === tunnelID);
  const availablePeers =
    selected?.peers.filter((peer) => peerPerformanceReady(peer, selected)) ??
    [];

  useEffect(() => {
    if (!open) return;
    const nextTunnel =
      initialTunnel && tunnelPerformanceReady(initialTunnel)
        ? initialTunnel
        : ready[0];
    setTunnelID(nextTunnel?.id ?? "");
    setNodeID(
      nextTunnel?.peers.find((peer) => peerPerformanceReady(peer, nextTunnel))
        ?.node_id ?? "",
    );
    setTargetMbps(100);
    setDurationSeconds(10);
  }, [initialTunnel, open, ready]);

  const start = useMutation({
    mutationFn: () =>
      api<WireGuardPerformanceTest>("/api/wireguard/performance-tests", {
        method: "POST",
        ...jsonBody({
          tunnel_id: tunnelID,
          node_id: nodeID,
          target_mbps: targetMbps,
          duration_seconds: durationSeconds,
        }),
      }),
    onSuccess: () => {
      toast.success(t("WireGuard 性能测试已排队"));
      onStarted();
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form
          className="space-y-5"
          onSubmit={(event) => {
            event.preventDefault();
            start.mutate();
          }}
        >
          <DialogHeader>
            <DialogTitle>{t("WireGuard 链路测试")}</DialogTitle>
            <DialogDescription>
              {t("公网 TCP、隧道 TCP 与隧道 UDP")}
            </DialogDescription>
          </DialogHeader>
          <Field label={t("隧道")} id="performance-tunnel">
            <Select
              value={tunnelID}
              onValueChange={(value) => {
                setTunnelID(value);
                const tunnel = ready.find((item) => item.id === value);
                setNodeID(
                  tunnel?.peers.find((peer) =>
                    peerPerformanceReady(peer, tunnel),
                  )?.node_id ?? "",
                );
              }}
            >
              <SelectTrigger id="performance-tunnel" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {ready.map((tunnel) => (
                  <SelectItem key={tunnel.id} value={tunnel.id}>
                    {tunnel.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>
          <Field label={t("边缘节点")} id="performance-node">
            <Select value={nodeID} onValueChange={setNodeID}>
              <SelectTrigger id="performance-node" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {availablePeers.map((peer) => (
                  <SelectItem key={peer.node_id} value={peer.node_id}>
                    {peer.node_name} · {peer.address}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label={t("UDP 目标带宽（Mbps）")} id="performance-mbps">
              <Input
                id="performance-mbps"
                type="number"
                required
                min={1}
                max={10000}
                value={targetMbps}
                onChange={(event) => setTargetMbps(Number(event.target.value))}
              />
            </Field>
            <Field label={t("单项时长（秒）")} id="performance-duration">
              <Input
                id="performance-duration"
                type="number"
                required
                min={3}
                max={60}
                value={durationSeconds}
                onChange={(event) =>
                  setDurationSeconds(Number(event.target.value))
                }
              />
            </Field>
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
              disabled={start.isPending || !tunnelID || !nodeID}
            >
              {start.isPending ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <Gauge />
              )}
              {t("开始测试")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function TunnelDetailDialog({
  tunnel,
  onOpenChange,
  onUninstall,
}: {
  tunnel: WireGuardTunnel | null;
  onOpenChange: (open: boolean) => void;
  onUninstall: (tunnel: WireGuardTunnel) => void;
}) {
  if (!tunnel) return null;
  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-4xl">
        <DialogHeader>
          <DialogTitle>{tunnel.name}</DialogTitle>
          <DialogDescription>
            {tunnel.endpoint_host}:{tunnel.listen_port} · {tunnel.address_cidr}{" "}
            · r{tunnel.revision}
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3 text-sm sm:grid-cols-3">
          <Fact label={t("源站隧道 IP")} value={tunnel.origin_address} />
          <Fact
            label={t("源站公钥")}
            value={shortHash(tunnel.origin_public_key)}
          />
          <Fact label={t("性能端口")} value={String(tunnel.performance_port)} />
          <Fact
            label={t("源站出口上限")}
            value={formatEgressLimit(tunnel.origin_egress_limit_mbps)}
          />
          <Fact label="MTU" value={String(tunnel.mtu)} />
          <Fact
            label={t("保活间隔")}
            value={`${tunnel.persistent_keepalive_seconds}s`}
          />
          <Fact
            label={t("源站应用时间")}
            value={formatDateTime(tunnel.origin_configured_at)}
          />
        </div>
        <Panel>
          <Table className="min-w-[950px]">
            <TableHeader>
              <TableRow>
                <TableHead className="pl-5">{t("节点")}</TableHead>
                <TableHead>{t("隧道 IP")}</TableHead>
                <TableHead>{t("修订")}</TableHead>
                <TableHead>{t("公钥")}</TableHead>
                <TableHead>{t("边缘出口上限")}</TableHead>
                <TableHead>{t("接收 / 发送")}</TableHead>
                <TableHead className="pr-5">{t("最近握手")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {tunnel.peers.map((peer) => (
                <TableRow key={peer.node_id}>
                  <TableCell className="pl-5">
                    <div className="font-medium">{peer.node_name}</div>
                    <div className="font-mono text-xs text-muted-foreground">
                      {peer.node_public_ipv4}
                    </div>
                  </TableCell>
                  <TableCell className="font-mono">{peer.address}</TableCell>
                  <TableCell>
                    <StatusBadge
                      status={
                        peerApplied(peer, tunnel)
                          ? "ready"
                          : peer.last_error
                            ? "failed"
                            : "pending"
                      }
                      label={`r${peer.applied_revision}`}
                    />
                    {peer.last_error ? (
                      <p className="mt-1 max-w-64 text-xs text-destructive">
                        {peer.last_error}
                      </p>
                    ) : null}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {shortHash(peer.public_key)}
                  </TableCell>
                  <TableCell className="text-xs">
                    {formatEgressLimit(peer.edge_egress_limit_mbps)}
                  </TableCell>
                  <TableCell className="text-xs">
                    {formatBytes(peer.rx_bytes)} / {formatBytes(peer.tx_bytes)}
                  </TableCell>
                  <TableCell className="pr-5">
                    {formatDateTime(peer.latest_handshake_at)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Panel>
        <DialogFooter>
          <Button variant="outline" onClick={() => onUninstall(tunnel)}>
            <Unplug />
            {t("生成源站卸载命令")}
          </Button>
          <Button onClick={() => onOpenChange(false)}>{t("关闭")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function CommandDialog({
  command,
  onOpenChange,
}: {
  command: CommandState | null;
  onOpenChange: (open: boolean) => void;
}) {
  if (!command) return null;
  return (
    <Dialog open onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-3xl">
        <DialogHeader>
          <DialogTitle>{command.title}</DialogTitle>
          <DialogDescription>
            {command.expiresAt
              ? t("一次性令牌有效至 {value0}", {
                  value0: formatDateTime(command.expiresAt),
                })
              : t("仅移除该隧道的受管配置")}
          </DialogDescription>
        </DialogHeader>
        <div className="relative rounded-lg border bg-muted/40 p-4 pr-12">
          <pre className="whitespace-pre-wrap break-all font-mono text-xs leading-5">
            {command.command}
          </pre>
          <div className="absolute right-3 top-3">
            <CopyButton value={command.command} label={t("复制命令")} />
          </div>
        </div>
        <DialogFooter>
          <Button onClick={() => onOpenChange(false)}>{t("关闭")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function PerformanceTable({ tests }: { tests: WireGuardPerformanceTest[] }) {
  return (
    <Panel className="rounded-b-none">
      <Table className="min-w-[1180px]">
        <TableHeader>
          <TableRow>
            <TableHead className="pl-5">{t("时间")}</TableHead>
            <TableHead>{t("隧道 / 节点")}</TableHead>
            <TableHead>{t("公网 TCP")}</TableHead>
            <TableHead>{t("隧道 TCP")}</TableHead>
            <TableHead>{t("TCP 差异")}</TableHead>
            <TableHead>{t("隧道 UDP")}</TableHead>
            <TableHead>{t("丢包 / 抖动")}</TableHead>
            <TableHead className="pr-5">{t("状态")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {tests.map((test) => {
            const direct = test.result?.direct_tcp;
            const tunneled = test.result?.wireguard_tcp;
            const udp = test.result?.wireguard_udp;
            const difference =
              direct && direct.mbps > 0 && tunneled
                ? tunneled.mbps / direct.mbps - 1
                : null;
            return (
              <TableRow key={test.id}>
                <TableCell className="pl-5">
                  {formatDateTime(test.created_at)}
                </TableCell>
                <TableCell>
                  <div className="font-medium">{test.tunnel_name}</div>
                  <div className="text-xs text-muted-foreground">
                    {test.node_name}
                  </div>
                </TableCell>
                <TableCell>{tcpMetric(direct)}</TableCell>
                <TableCell>{tcpMetric(tunneled)}</TableCell>
                <TableCell>
                  {difference == null ? "--" : formatPercent(difference, 1)}
                </TableCell>
                <TableCell>
                  {udp
                    ? `${metricNumber(udp.mbps)} Mbps / ${formatNumber(udp.target_mbps)} Mbps`
                    : "--"}
                </TableCell>
                <TableCell>
                  {udp
                    ? `${formatPercent(udp.loss_percent / 100, 2)} / ${metricNumber(udp.jitter_ms)} ms`
                    : "--"}
                </TableCell>
                <TableCell className="pr-5">
                  <StatusBadge status={test.status} />
                  {test.error ? (
                    <p className="mt-1 max-w-72 text-xs text-destructive">
                      {test.error}
                    </p>
                  ) : null}
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </Panel>
  );
}

function Summary({
  icon,
  label,
  value,
  detail,
}: {
  icon: ReactNode;
  label: string;
  value: string;
  detail: string;
}) {
  return (
    <div className="flex min-h-28 items-center gap-3 p-4 sm:p-5">
      <span className="flex size-9 shrink-0 items-center justify-center rounded-md border bg-muted/40 text-muted-foreground [&>svg]:size-4">
        {icon}
      </span>
      <div className="min-w-0">
        <div className="text-xs text-muted-foreground">{label}</div>
        <div className="mt-1 text-xl font-semibold tabular-nums">{value}</div>
        <div className="mt-1 truncate text-xs text-muted-foreground">
          {detail}
        </div>
      </div>
    </div>
  );
}

function IconAction({
  label,
  onClick,
  disabled,
  children,
}: {
  label: string;
  onClick: () => void;
  disabled?: boolean;
  children: ReactNode;
}) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          aria-label={label}
          disabled={disabled}
          onClick={onClick}
        >
          {children}
        </Button>
      </TooltipTrigger>
      <TooltipContent>{label}</TooltipContent>
    </Tooltip>
  );
}

function Field({
  label,
  id,
  children,
}: {
  label: string;
  id: string;
  children: ReactNode;
}) {
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  );
}

function Fact({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-md border px-3 py-2">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="mt-1 truncate font-mono text-xs">{value}</div>
    </div>
  );
}

function tunnelDraft(tunnel: WireGuardTunnel | null): TunnelDraft {
  return tunnel
    ? {
        name: tunnel.name,
        endpoint_host: tunnel.endpoint_host,
        listen_port: tunnel.listen_port,
        address_cidr: tunnel.address_cidr,
        mtu: tunnel.mtu,
        persistent_keepalive_seconds: tunnel.persistent_keepalive_seconds,
        performance_port: tunnel.performance_port,
        origin_egress_limit_mbps: tunnel.origin_egress_limit_mbps,
        node_ids: tunnel.peers.map((peer) => peer.node_id),
        edge_egress_limits_mbps: Object.fromEntries(
          tunnel.peers.map((peer) => [
            peer.node_id,
            peer.edge_egress_limit_mbps,
          ]),
        ),
      }
    : {
        name: "",
        endpoint_host: "",
        listen_port: 51820,
        address_cidr: "",
        mtu: 1420,
        persistent_keepalive_seconds: 25,
        performance_port: 5201,
        origin_egress_limit_mbps: 0,
        node_ids: [],
        edge_egress_limits_mbps: {},
      };
}

function originReady(tunnel: WireGuardTunnel) {
  return Boolean(
    tunnel.origin_public_key &&
    tunnel.origin_configured_revision === tunnel.revision,
  );
}

function peerApplied(peer: WireGuardPeer, tunnel: WireGuardTunnel) {
  return Boolean(
    peer.public_key &&
    !peer.last_error &&
    peer.applied_revision === tunnel.revision,
  );
}

function tunnelReady(tunnel: WireGuardTunnel) {
  return (
    originReady(tunnel) &&
    tunnel.peers.length > 0 &&
    tunnel.peers.every((peer) => peerApplied(peer, tunnel))
  );
}

function peerPerformanceReady(peer: WireGuardPeer, tunnel: WireGuardTunnel) {
  return peerApplied(peer, tunnel) && handshakeFresh(peer.latest_handshake_at);
}

function tunnelPerformanceReady(tunnel: WireGuardTunnel) {
  return (
    tunnelReady(tunnel) &&
    tunnel.peers.some((peer) => peerPerformanceReady(peer, tunnel))
  );
}

function latestHandshake(tunnel: WireGuardTunnel) {
  const values = tunnel.peers
    .map((peer) => peer.latest_handshake_at)
    .filter((value): value is string => Boolean(value))
    .sort();
  return values.at(-1);
}

function handshakeFresh(value?: string) {
  if (!value) return false;
  const timestamp = new Date(value).getTime();
  const age = Date.now() - timestamp;
  return Number.isFinite(timestamp) && age >= -30_000 && age <= 180_000;
}

function formatEgressLimit(limitMbps: number) {
  return limitMbps > 0 ? `${formatNumber(limitMbps)} Mbps` : t("不限速");
}

function metricNumber(value: number) {
  return Number(value).toLocaleString(undefined, {
    minimumFractionDigits: 1,
    maximumFractionDigits: 2,
  });
}

function tcpMetric(value?: WireGuardTCPMeasurement) {
  if (!value) return "--";
  return `${metricNumber(value.mbps)} Mbps / ${formatNumber(value.retransmits)} retx`;
}
