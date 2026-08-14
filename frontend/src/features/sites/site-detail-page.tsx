import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ArrowLeft,
  ArrowRight,
  KeyRound,
  LoaderCircle,
  Network,
  Plus,
  Rocket,
  Save,
  ShieldCheck,
  Trash2,
  X,
} from "lucide-react";
import { useEffect, useId, useState, type FormEvent } from "react";
import { useNavigate, useParams } from "react-router";
import { toast } from "sonner";
import { ConfirmDialog } from "@/components/confirm-dialog";
import { CopyButton } from "@/components/copy-button";
import { ListPagination } from "@/components/list-pagination";
import {
  normalOriginProbeCount,
  OriginConnectionsTable,
} from "@/components/origin-connections-table";
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
import { Separator } from "@/components/ui/separator";
import { Switch } from "@/components/ui/switch";
import { Table, TableBody, TableCell, TableRow } from "@/components/ui/table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Textarea } from "@/components/ui/textarea";
import { api, ApiError, errorMessage } from "@/lib/api";
import {
  formatBytes,
  formatDateTime,
  formatNumber,
  formatPercent,
} from "@/lib/format";
import { useListPagination } from "@/hooks/use-list-pagination";
import type {
  DeploymentTask,
  Node,
  OriginHTTPVersion,
  OriginHealthCheckMethod,
  PublishStatus,
  Settings,
  Site,
  SiteMinuteMetric,
  SiteOriginConnections as SiteOriginConnectionsData,
  TCPForward,
  WireGuardTunnel,
} from "@/lib/types";
import { t, useI18n } from "@/lib/i18n";
import { activeTask, taskMatchesCurrentSite } from "./publish-status";
interface SiteDraft {
  name: string;
  domains: string;
  node_ids: string[];
  backup_node_ids: string[];
  primary_url: string;
  primary_host: string;
  primary_sni: string;
  primary_http_version: OriginHTTPVersion;
  primary_health_method: OriginHealthCheckMethod;
  primary_health_path: string;
  primary_wireguard_tunnel_id: string;
  backup_enabled: boolean;
  backup_url: string;
  backup_host: string;
  backup_sni: string;
  backup_http_version: OriginHTTPVersion;
  backup_health_method: OriginHealthCheckMethod;
  backup_health_path: string;
  backup_wireguard_tunnel_id: string;
  passthrough: boolean;
  request_body_buffering: boolean;
  origin_response_buffering: boolean;
  dynamic_compression_enabled: boolean;
  compression_excluded_mime_types: string;
  http3_enabled: boolean;
  client_max_body_size_mb: number;
  client_keepalive_timeout_seconds: number;
  read_write_timeout_seconds: number;
  inherit_dns_ttl: boolean;
  dns_ttl_seconds: number;
  tcp_only: boolean;
  tcp_forwards: TCPForward[];
  enabled: boolean;
}
interface TLSStatus {
  certificate_task: DeploymentTask | null;
  published_after_certificate: boolean;
}
interface Allowlist {
  site_id: string;
  ipv4_cidrs: string[];
  nodes?: Array<{
    node_id: string;
    node_name: string;
    ipv4_cidr: string;
    assignment: "current" | "current_and_published" | "published_only";
  }>;
  note: string;
}
export function SiteDetailPage() {
  useI18n();
  const { siteId } = useParams();
  const isNew = !siteId;
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const sites = useQuery({
    queryKey: ["sites"],
    queryFn: () => api<Site[]>("/api/sites"),
  });
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => api<Node[]>("/api/nodes"),
  });
  const wireGuardTunnels = useQuery({
    queryKey: ["wireguard-tunnels"],
    queryFn: () => api<WireGuardTunnel[]>("/api/wireguard/tunnels"),
  });
  const settings = useQuery({
    queryKey: ["settings"],
    queryFn: () => api<Settings>("/api/settings"),
  });
  const site = sites.data?.find((item) => item.id === siteId);
  const [draft, setDraft] = useState<SiteDraft>(() => emptyDraft(60));
  const [baseline, setBaseline] = useState("");
  const [loadedKey, setLoadedKey] = useState("");
  const [discardOpen, setDiscardOpen] = useState(false);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [allowlistOpen, setAllowlistOpen] = useState(false);
  const [tlsPendingOpen, setTlsPendingOpen] = useState(false);
  const [checkingTLS, setCheckingTLS] = useState(false);
  const globalTTL = settings.data?.dns.default_ttl_seconds ?? 60;
  const dirty = Boolean(baseline && JSON.stringify(draft) !== baseline);
  const encodedID = encodeURIComponent(siteId ?? "");
  useEffect(() => {
    if (!settings.isFetched) return;
    const key = isNew ? "new" : site?.id;
    if (!key || key === loadedKey) return;
    const next = site ? draftFromSite(site, globalTTL) : emptyDraft(globalTTL);
    setDraft(next);
    setBaseline(JSON.stringify(next));
    setLoadedKey(key);
  }, [globalTTL, isNew, loadedKey, settings.isFetched, site]);
  useEffect(() => {
    if (!dirty) return;
    const warn = (event: BeforeUnloadEvent) => event.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);
  const tls = useQuery({
    queryKey: ["site-tls", siteId],
    queryFn: () => api<TLSStatus>(`/api/sites/${encodedID}/tls-status`),
    enabled: !isNew && Boolean(site),
    refetchInterval: (query) =>
      activeTask(query.state.data?.certificate_task) ? 2_000 : 20_000,
  });
  const publish = useQuery({
    queryKey: ["site-publish", siteId],
    queryFn: () => api<PublishStatus>(`/api/sites/${encodedID}/publish-status`),
    enabled: !isNew && Boolean(site),
    refetchInterval: (query) =>
      activeTask(query.state.data?.task) ? 2_000 : 20_000,
  });
  const deletion = useQuery({
    queryKey: ["site-deletion", siteId],
    queryFn: () => api<PublishStatus>(`/api/sites/${encodedID}/delete-status`),
    enabled: Boolean(site?.deleting),
    refetchInterval: (query) =>
      activeTask(query.state.data?.task) ? 2_000 : false,
  });
  const allowlist = useQuery({
    queryKey: ["site-allowlist", siteId],
    queryFn: () => api<Allowlist>(`/api/sites/${encodedID}/origin-allowlist`),
    enabled: allowlistOpen && !isNew,
  });
  const metrics = useQuery({
    queryKey: ["site-metrics", siteId],
    queryFn: () => api<SiteMinuteMetric[]>(`/api/sites/${encodedID}/metrics`),
    enabled: !isNew && Boolean(site),
    refetchInterval: 30_000,
  });
  const originConnections = useQuery({
    queryKey: ["site-origin-connections", siteId],
    queryFn: () =>
      api<SiteOriginConnectionsData>(
        `/api/sites/${encodedID}/origin-connections`,
      ),
    enabled: !isNew && Boolean(site) && !site?.tcp_only,
    refetchInterval: 5_000,
  });
  useEffect(() => {
    if (deletion.data?.task?.status !== "succeeded") return;
    toast.success(t("站点已安全删除"));
    void queryClient.invalidateQueries({
      queryKey: ["sites"],
    });
    navigate("/sites", {
      replace: true,
    });
  }, [deletion.data?.task?.status, navigate, queryClient]);
  const save = useMutation({
    mutationFn: () =>
      api<Site>(isNew ? "/api/sites" : `/api/sites/${encodedID}`, {
        method: isNew ? "POST" : "PUT",
        body: JSON.stringify(sitePayload(draft)),
      }),
    onSuccess: (saved) => {
      const next = draftFromSite(saved, globalTTL);
      setDraft(next);
      setBaseline(JSON.stringify(next));
      setLoadedKey(saved.id);
      queryClient.setQueryData<Site[]>(["sites"], (current) => {
        if (!current) return current;
        if (!current.some((item) => item.id === saved.id)) {
          return [...current, saved];
        }
        return current.map((item) => (item.id === saved.id ? saved : item));
      });
      void queryClient.invalidateQueries({
        queryKey: ["sites"],
      });
      void queryClient.invalidateQueries({
        queryKey: ["site-tls", saved.id],
      });
      void queryClient.invalidateQueries({
        queryKey: ["site-publish", saved.id],
      });
      void queryClient.invalidateQueries({
        queryKey: ["site-allowlist", saved.id],
      });
      void queryClient.invalidateQueries({
        queryKey: ["site-origin-connections", saved.id],
      });
      toast.success(
        isNew && siteNeedsCertificate(saved)
          ? t("站点已创建，TLS 证书正在自动申请")
          : isNew
            ? t("站点已创建")
            : t("站点配置已保存"),
      );
      if (isNew)
        navigate(`/sites/${encodeURIComponent(saved.id)}`, {
          replace: true,
        });
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const operation = useMutation({
    mutationFn: ({ path, body }: { path: string; body?: unknown }) =>
      api<DeploymentTask>(path, {
        method: "POST",
        body: body === undefined ? undefined : JSON.stringify(body),
      }),
    onSuccess: (_, input) => {
      toast.success(
        input.path.endsWith("certificate")
          ? t("TLS 签发已排队")
          : t("站点发布已启动"),
      );
      void queryClient.invalidateQueries({
        queryKey: ["site-tls", siteId],
      });
      void queryClient.invalidateQueries({
        queryKey: ["site-publish", siteId],
      });
      void queryClient.invalidateQueries({
        queryKey: ["sites"],
      });
    },
    onError: (error, input) => {
      if (
        input.path.endsWith("/publish") &&
        error instanceof ApiError &&
        error.status === 409 &&
        error.data &&
        typeof error.data === "object" &&
        "certificate_task" in error.data
      ) {
        const task = (
          error.data as {
            certificate_task?: DeploymentTask;
          }
        ).certificate_task;
        if (task) {
          queryClient.setQueryData<TLSStatus>(["site-tls", siteId], {
            certificate_task: task,
            published_after_certificate: false,
          });
          setTlsPendingOpen(true);
          return;
        }
      }
      toast.error(errorMessage(error));
    },
  });
  const deleteSite = useMutation({
    mutationFn: () =>
      api<PublishStatus>(`/api/sites/${encodedID}`, {
        method: "DELETE",
        body: JSON.stringify({
          confirmation: site?.name,
        }),
      }),
    onSuccess: (status) => {
      queryClient.setQueryData(["site-deletion", siteId], status);
      void queryClient.invalidateQueries({
        queryKey: ["sites"],
      });
      toast.success(t("安全删除已启动"));
      setDeleteOpen(false);
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    save.mutate();
  }
  function goBack() {
    if (dirty) setDiscardOpen(true);
    else navigate("/sites");
  }
  async function publishSite() {
    if (!site) return;
    if (!site.enabled || !siteNeedsCertificate(site)) {
      operation.mutate({
        path: `/api/sites/${encodedID}/publish`,
      });
      return;
    }
    setCheckingTLS(true);
    try {
      let status = await api<TLSStatus>(`/api/sites/${encodedID}/tls-status`);
      queryClient.setQueryData(["site-tls", siteId], status);
      if (status.certificate_task?.status === "succeeded") {
        operation.mutate({
          path: `/api/sites/${encodedID}/publish`,
        });
        return;
      }
      if (!activeTask(status.certificate_task)) {
        const task = await api<DeploymentTask>(
          `/api/sites/${encodedID}/certificate`,
          {
            method: "POST",
          },
        );
        status = {
          certificate_task: task,
          published_after_certificate: false,
        };
        queryClient.setQueryData(["site-tls", siteId], status);
      }
      setTlsPendingOpen(true);
    } catch (error) {
      toast.error(errorMessage(error));
    } finally {
      setCheckingTLS(false);
    }
  }
  const loading =
    sites.isLoading ||
    nodes.isLoading ||
    settings.isLoading ||
    wireGuardTunnels.isLoading ||
    !loadedKey;
  return (
    <>
      <PageHeader
        title={isNew ? t("添加站点") : (site?.name ?? t("站点配置"))}
        description={
          site
            ? `${site.domains.join(", ") || t("无 HTTP 域名")} · ${site.id}`
            : t("创建新的边缘站点配置")
        }
        actions={
          <Button variant="outline" onClick={goBack}>
            <ArrowLeft />
            {t("返回站点")}
          </Button>
        }
      />
      <PageBody>
        {loading ? <PageLoading /> : null}
        {sites.error ||
        nodes.error ||
        settings.error ||
        wireGuardTunnels.error ? (
          <PageError
            error={
              sites.error ||
              nodes.error ||
              settings.error ||
              wireGuardTunnels.error
            }
          />
        ) : null}
        {!isNew && sites.data && !site ? (
          <EmptyState
            title={t("未找到站点")}
            description={t("该站点可能已被删除")}
          />
        ) : null}
        {!loading && (isNew || site) ? (
          <form
            className="grid min-w-0 gap-5 xl:grid-cols-[minmax(0,1fr)_22rem]"
            onSubmit={submit}
          >
            <div className="min-w-0 space-y-5">
              {site?.deleting ? (
                <Alert variant="destructive">
                  <AlertTitle>{t("站点正在删除")}</AlertTitle>
                  <AlertDescription>
                    {deletion.data?.task?.detail ||
                      t("配置已锁定，正在撤销 DNS 并等待边缘节点确认。")}
                  </AlertDescription>
                </Alert>
              ) : null}
              <BasicSettings draft={draft} setDraft={setDraft} />
              <TrafficSettings
                draft={draft}
                setDraft={setDraft}
                tunnels={wireGuardTunnels.data ?? []}
              />
              {site && !site.tcp_only ? (
                <SiteOriginConnections
                  data={originConnections.data}
                  error={originConnections.error}
                  loading={originConnections.isLoading}
                />
              ) : null}
              <NodeSelector
                nodes={nodes.data ?? []}
                primarySelected={draft.node_ids}
                backupSelected={draft.backup_node_ids}
                onChange={(node_ids, backup_node_ids) =>
                  setDraft({
                    ...draft,
                    node_ids,
                    backup_node_ids,
                  })
                }
              />
              <TCPForwards draft={draft} setDraft={setDraft} />
            </div>
            <aside className="space-y-4 xl:sticky xl:top-16 xl:self-start">
              <Card>
                <CardHeader>
                  <CardTitle>{t("配置摘要")}</CardTitle>
                  <CardDescription>
                    {dirty ? t("有未保存的更改") : t("配置已同步")}
                  </CardDescription>
                </CardHeader>
                <CardContent className="grid gap-3 text-sm">
                  <Fact
                    label={t("流量模式")}
                    value={
                      draft.tcp_only
                        ? t("仅 TCP / TLS")
                        : draft.tcp_forwards.length
                          ? "HTTP + TCP"
                          : "HTTP / gRPC / WS"
                    }
                  />
                  {!draft.tcp_only ? (
                    <>
                      <Fact
                        label={t("回源链路")}
                        value={
                          draft.primary_wireguard_tunnel_id
                            ? `${t("隧道")} · ${
                                wireGuardTunnels.data?.find(
                                  (tunnel) =>
                                    tunnel.id ===
                                    draft.primary_wireguard_tunnel_id,
                                )?.name ?? draft.primary_wireguard_tunnel_id
                              }`
                            : t("公网直连")
                        }
                      />
                      <Fact
                        label="HTTP/3 / QUIC"
                        value={draft.http3_enabled ? t("已开启") : t("已关闭")}
                      />
                      <Fact
                        label={t("动态压缩")}
                        value={
                          draft.dynamic_compression_enabled
                            ? t("已开启")
                            : t("已关闭")
                        }
                      />
                    </>
                  ) : null}
                  <Fact
                    label={t("主节点")}
                    value={t("{value0} 个", {
                      value0: draft.node_ids.length,
                    })}
                  />
                  <Fact
                    label={t("备用节点")}
                    value={t("{value0} 个", {
                      value0: draft.backup_node_ids.length,
                    })}
                  />
                  <Fact
                    label="DNS TTL"
                    value={
                      draft.inherit_dns_ttl
                        ? t("{value0} 秒（全局）", {
                            value0: globalTTL,
                          })
                        : t("{value0} 秒", {
                            value0: draft.dns_ttl_seconds,
                          })
                    }
                  />
                  <Fact
                    label={t("TCP 端口")}
                    value={
                      draft.tcp_forwards.length
                        ? draft.tcp_forwards
                            .map((item) => item.listen_port || "--")
                            .join(", ")
                        : t("未配置")
                    }
                  />
                  <Button
                    type="submit"
                    disabled={save.isPending || site?.deleting}
                  >
                    {save.isPending ? (
                      <LoaderCircle className="animate-spin" />
                    ) : (
                      <Save />
                    )}
                    {isNew ? t("创建站点") : t("保存更改")}
                  </Button>
                </CardContent>
              </Card>
              <CompressionPerformance metrics={metrics.data} />
              <OriginPerformance metrics={metrics.data} />
              {site ? (
                <SiteOperations
                  site={site}
                  nodes={nodes.data ?? []}
                  tls={tls.data}
                  publish={publish.data}
                  deletion={deletion.data}
                  pending={operation.isPending || checkingTLS}
                  onPublish={() => void publishSite()}
                  onCertificate={() =>
                    operation.mutate({
                      path: `/api/sites/${encodedID}/certificate`,
                    })
                  }
                  onManageCache={() => navigate(`/cache?site_id=${encodedID}`)}
                  onAllowlist={() => setAllowlistOpen(true)}
                  onDelete={() => setDeleteOpen(true)}
                />
              ) : null}
            </aside>
          </form>
        ) : null}
      </PageBody>
      <ConfirmDialog
        open={discardOpen}
        onOpenChange={setDiscardOpen}
        title={t("放弃未保存的更改？")}
        description={t("当前站点表单包含未保存内容。")}
        confirmLabel={t("放弃更改")}
        destructive
        onConfirm={() => navigate("/sites")}
      />
      <ConfirmDialog
        open={deleteOpen}
        onOpenChange={setDeleteOpen}
        title={site?.deleting ? t("重试安全删除") : t("安全删除站点")}
        description={t(
          "控制面会撤销托管 DNS、从所有边缘节点移除配置，并清理证书材料。",
        )}
        confirmation={site?.name}
        confirmLabel={site?.deleting ? t("重试删除") : t("开始删除")}
        destructive
        busy={deleteSite.isPending}
        onConfirm={async () => {
          await deleteSite.mutateAsync();
        }}
      />
      <Dialog open={tlsPendingOpen} onOpenChange={setTlsPendingOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("正在申请 TLS 证书")}</DialogTitle>
            <DialogDescription>
              {t(
                "TLS 证书申请成功后才能发布站点。系统正在后台完成 DNS-01 验证，请稍后再次发布。",
              )}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button type="button" onClick={() => setTlsPendingOpen(false)}>
              {t("知道了")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <AllowlistDialog
        open={allowlistOpen}
        onOpenChange={setAllowlistOpen}
        data={allowlist.data}
        loading={allowlist.isLoading}
      />
    </>
  );
}

function SiteOriginConnections({
  data,
  error,
  loading,
}: {
  data?: SiteOriginConnectionsData;
  error: Error | null;
  loading: boolean;
}) {
  const probes = data?.nodes.flatMap((node) => node.probes) ?? [];
  const normal = normalOriginProbeCount(probes);
  return (
    <Card className="min-w-0">
      <CardHeader>
        <CardTitle>{t("回源连接")}</CardTitle>
        <CardDescription>
          {data
            ? t("{value0} 个连接池 · {value1} 个正常", {
                value0: probes.length,
                value1: normal,
              })
            : loading
              ? t("正在加载")
              : errorMessage(error)}
        </CardDescription>
      </CardHeader>
      <CardContent className="min-w-0 p-0">
        {error ? (
          <p className="border-t px-6 py-4 text-sm text-destructive">
            {errorMessage(error)}
          </p>
        ) : null}
        {!error && !data ? (
          <p className="border-t px-6 py-4 text-sm text-muted-foreground">
            {t("正在加载")}
          </p>
        ) : null}
        {data?.nodes.length === 0 ? (
          <p className="border-t px-6 py-4 text-sm text-muted-foreground">
            {t("当前配置未分配边缘节点")}
          </p>
        ) : null}
        {data?.nodes.map((node) => (
          <section className="min-w-0 border-t px-6 py-4" key={node.node_id}>
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div className="min-w-0">
                <h3 className="truncate text-sm font-medium">
                  {node.node_name}
                </h3>
                <p className="mt-1 text-xs text-muted-foreground">
                  <span className="font-mono">{node.public_ipv4}</span>
                  {node.collected_at ? (
                    <>
                      {" · "}
                      {t("采集于 ")}
                      {formatDateTime(node.collected_at)}
                    </>
                  ) : null}
                </p>
              </div>
              <div className="flex flex-wrap items-center justify-end gap-2">
                <StatusBadge status={node.status} />
                {!node.available ? (
                  <StatusBadge status="pending" label={t("等待上报")} />
                ) : node.stale ? (
                  <StatusBadge status="failed" label={t("数据过期")} />
                ) : (
                  <StatusBadge status="active" label={t("数据正常")} />
                )}
              </div>
            </div>
            {node.probes.length ? (
              <div className="mt-4">
                <OriginConnectionsTable
                  probes={node.probes}
                  contextHeading={t("角色")}
                  contexts={(probe) =>
                    probe.references.map((reference) => ({
                      key: `${reference.site_id}-${reference.role}`,
                      label:
                        reference.role === "primary" ? t("主源") : t("备源"),
                    }))
                  }
                />
              </div>
            ) : (
              <p className="mt-3 text-sm text-muted-foreground">
                {node.available
                  ? t("等待节点上报回源连接")
                  : t(node.unavailable_reason ?? "等待上报")}
              </p>
            )}
          </section>
        ))}
      </CardContent>
    </Card>
  );
}

function CompressionPerformance({ metrics }: { metrics?: SiteMinuteMetric[] }) {
  if (!metrics) return null;
  const requests = metrics.reduce(
    (total, metric) => total + metric.requests,
    0,
  );
  const compressed = metrics.reduce(
    (total, metric) => total + (metric.compressed_requests ?? 0),
    0,
  );
  const saved = metrics.reduce(
    (total, metric) => total + (metric.compression_saved_bytes ?? 0),
    0,
  );
  const gzip = metrics.reduce(
    (total, metric) => total + (metric.gzip_requests ?? 0),
    0,
  );
  const brotli = metrics.reduce(
    (total, metric) => total + (metric.brotli_requests ?? 0),
    0,
  );
  const zstd = metrics.reduce(
    (total, metric) => total + (metric.zstd_requests ?? 0),
    0,
  );
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("压缩统计")}</CardTitle>
        <CardDescription>{t("最近 24 小时")}</CardDescription>
      </CardHeader>
      <CardContent className="grid grid-cols-2 gap-x-4 gap-y-3">
        <Fact label={t("节省流量")} value={formatBytes(saved)} />
        <Fact
          label={t("压缩命中率")}
          value={requests ? formatPercent(compressed / requests, 1) : "--"}
        />
        <Fact label="Zstandard" value={formatNumber(zstd)} />
        <Fact label="Brotli" value={formatNumber(brotli)} />
        <Fact label="Gzip" value={formatNumber(gzip)} />
        <Fact label={t("压缩响应")} value={formatNumber(compressed)} />
      </CardContent>
    </Card>
  );
}

function OriginPerformance({ metrics }: { metrics?: SiteMinuteMetric[] }) {
  const samples =
    metrics?.reduce((total, metric) => total + metric.upstream_samples, 0) ?? 0;
  if (!samples) return null;
  const reused =
    metrics?.reduce((total, metric) => total + metric.upstream_reused, 0) ?? 0;
  const weighted = (
    field: keyof SiteMinuteMetric,
    sampleField:
      | "upstream_samples"
      | "upstream_header_samples"
      | "upstream_response_samples",
  ) => {
    const count =
      metrics?.reduce((total, metric) => total + metric[sampleField], 0) ?? 0;
    if (!count) return 0;
    return (
      (metrics?.reduce(
        (total, metric) => total + Number(metric[field]) * metric[sampleField],
        0,
      ) ?? 0) / count
    );
  };
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("回源性能")}</CardTitle>
        <CardDescription>{t("最近 24 小时的真实请求样本")}</CardDescription>
      </CardHeader>
      <CardContent className="grid grid-cols-2 gap-x-4 gap-y-3">
        <Fact
          label={t("连接复用率")}
          value={formatPercent(reused / samples, 1)}
        />
        <Fact label={t("回源样本")} value={formatNumber(samples)} />
        <Fact
          label={t("平均建连")}
          value={`${formatNumber(weighted("upstream_connect_ms", "upstream_samples"))} ms`}
        />
        <Fact
          label={t("平均首字节")}
          value={`${formatNumber(weighted("upstream_header_ms", "upstream_header_samples"))} ms`}
        />
        <Fact
          label={t("平均完整响应")}
          value={`${formatNumber(weighted("upstream_response_ms", "upstream_response_samples"))} ms`}
        />
      </CardContent>
    </Card>
  );
}
function BasicSettings({
  draft,
  setDraft,
}: {
  draft: SiteDraft;
  setDraft: (draft: SiteDraft) => void;
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("基本配置")}</CardTitle>
        <CardDescription>{t("站点名称与入口域名")}</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-4 sm:grid-cols-2">
        <div className="sm:col-span-2 sm:max-w-xl">
          <Field label={t("站点名称")} id="site-name">
            <Input
              id="site-name"
              required
              maxLength={100}
              value={draft.name}
              onChange={(event) =>
                setDraft({
                  ...draft,
                  name: event.target.value,
                })
              }
            />
          </Field>
        </div>
        <div className="grid gap-2 sm:col-span-2">
          <Label htmlFor="site-domains">{t("域名")}</Label>
          <Textarea
            id="site-domains"
            rows={3}
            value={draft.domains}
            onChange={(event) =>
              setDraft({
                ...draft,
                domains: event.target.value,
              })
            }
            placeholder="cdn.example.com, static.example.com"
          />
          <p className="text-xs text-muted-foreground">
            {t("使用逗号或换行分隔")}
          </p>
        </div>
        <div className="flex items-center justify-between sm:col-span-2">
          <div>
            <Label htmlFor="site-enabled">{t("启用站点")}</Label>
            <p className="text-xs text-muted-foreground">
              {t("停用后下次发布会撤销入口服务")}
            </p>
          </div>
          <Switch
            id="site-enabled"
            checked={draft.enabled}
            onCheckedChange={(enabled) =>
              setDraft({
                ...draft,
                enabled,
              })
            }
          />
        </div>
      </CardContent>
    </Card>
  );
}
function TrafficSettings({
  draft,
  setDraft,
  tunnels,
}: {
  draft: SiteDraft;
  setDraft: (draft: SiteDraft) => void;
  tunnels: WireGuardTunnel[];
}) {
  const assignedNodeIDs = [...draft.node_ids, ...draft.backup_node_ids];
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("流量与源站")}</CardTitle>
        <CardDescription>{t("HTTP 系列协议或纯 TCP/TLS 转发")}</CardDescription>
      </CardHeader>
      <CardContent>
        <Tabs
          value={draft.tcp_only ? "tcp" : "http"}
          onValueChange={(value) =>
            setDraft({
              ...draft,
              tcp_only: value === "tcp",
              http3_enabled: value === "tcp" ? false : draft.http3_enabled,
            })
          }
        >
          <TabsList>
            <TabsTrigger value="http">HTTP / gRPC / WS</TabsTrigger>
            <TabsTrigger value="tcp">{t("仅 TCP / TLS")}</TabsTrigger>
          </TabsList>
          <TabsContent value="http" className="mt-5 space-y-5">
            <div className="flex items-start justify-between gap-4">
              <div className="space-y-1">
                <Label htmlFor="http3-enabled">HTTP/3 / QUIC</Label>
                <p className="max-w-2xl text-xs leading-5 text-muted-foreground">
                  {t(
                    "仅在支持的边缘节点使用 UDP 443；部分运营商可能限流，关闭时继续使用 HTTP/1.1 与 HTTP/2。",
                  )}
                </p>
              </div>
              <Switch
                id="http3-enabled"
                className="mt-0.5 shrink-0"
                checked={draft.http3_enabled}
                onCheckedChange={(http3_enabled) =>
                  setDraft({
                    ...draft,
                    http3_enabled,
                  })
                }
              />
            </div>
            <Separator />
            <OriginFields
              title={t("主源站")}
              required
              url={draft.primary_url}
              host={draft.primary_host}
              sni={draft.primary_sni}
              httpVersion={draft.primary_http_version}
              healthMethod={draft.primary_health_method}
              healthPath={draft.primary_health_path}
              wireGuardTunnelID={draft.primary_wireguard_tunnel_id}
              tunnels={tunnels}
              nodeIDs={assignedNodeIDs}
              onChange={(values) =>
                setDraft({
                  ...draft,
                  primary_url: values.url,
                  primary_host: values.host,
                  primary_sni: values.sni,
                  primary_http_version: values.httpVersion,
                  primary_health_method: values.healthMethod,
                  primary_health_path: values.healthPath,
                  primary_wireguard_tunnel_id: values.wireGuardTunnelID,
                })
              }
            />
            <Separator />
            <div className="flex items-center justify-between">
              <div>
                <Label htmlFor="backup-origin">{t("备用源站")}</Label>
                <p className="text-xs text-muted-foreground">
                  {t("主源站不可用时回退")}
                </p>
              </div>
              <Switch
                id="backup-origin"
                checked={draft.backup_enabled}
                onCheckedChange={(backup_enabled) =>
                  setDraft({
                    ...draft,
                    backup_enabled,
                  })
                }
              />
            </div>
            {draft.backup_enabled ? (
              <OriginFields
                title={t("备用源站")}
                url={draft.backup_url}
                host={draft.backup_host}
                sni={draft.backup_sni}
                httpVersion={draft.backup_http_version}
                healthMethod={draft.backup_health_method}
                healthPath={draft.backup_health_path}
                wireGuardTunnelID={draft.backup_wireguard_tunnel_id}
                tunnels={tunnels}
                nodeIDs={assignedNodeIDs}
                onChange={(values) =>
                  setDraft({
                    ...draft,
                    backup_url: values.url,
                    backup_host: values.host,
                    backup_sni: values.sni,
                    backup_http_version: values.httpVersion,
                    backup_health_method: values.healthMethod,
                    backup_health_path: values.healthPath,
                    backup_wireguard_tunnel_id: values.wireGuardTunnelID,
                  })
                }
              />
            ) : null}
            <Separator />
            <div className="grid gap-4 sm:grid-cols-3">
              <Field label={t("最大请求体（MiB）")} id="body-size">
                <Input
                  id="body-size"
                  type="number"
                  min={1}
                  max={1024}
                  required
                  value={draft.client_max_body_size_mb}
                  onChange={(event) =>
                    setDraft({
                      ...draft,
                      client_max_body_size_mb: Number(event.target.value),
                    })
                  }
                />
              </Field>
              <Field
                label={t("客户端保活（秒）")}
                id="client-keepalive-timeout"
              >
                <Input
                  id="client-keepalive-timeout"
                  type="number"
                  min={15}
                  max={3600}
                  required
                  value={draft.client_keepalive_timeout_seconds}
                  onChange={(event) =>
                    setDraft({
                      ...draft,
                      client_keepalive_timeout_seconds: Number(
                        event.target.value,
                      ),
                    })
                  }
                />
              </Field>
              <Field label={t("读写超时（秒）")} id="rw-timeout">
                <Input
                  id="rw-timeout"
                  type="number"
                  min={1}
                  required
                  value={draft.read_write_timeout_seconds}
                  onChange={(event) =>
                    setDraft({
                      ...draft,
                      read_write_timeout_seconds: Number(event.target.value),
                    })
                  }
                />
              </Field>
            </div>
            <Separator />
            <div className="grid gap-5 sm:grid-cols-2">
              <div className="flex items-start justify-between gap-4">
                <div className="space-y-1">
                  <Label htmlFor="request-body-buffering">
                    {t("请求体缓冲区")}
                  </Label>
                  <p className="text-xs leading-5 text-muted-foreground">
                    {t("关闭后边接收请求体边回源")}
                  </p>
                </div>
                <Switch
                  id="request-body-buffering"
                  className="mt-0.5 shrink-0"
                  checked={draft.request_body_buffering && !draft.passthrough}
                  disabled={draft.passthrough}
                  onCheckedChange={(request_body_buffering) =>
                    setDraft({
                      ...draft,
                      request_body_buffering,
                    })
                  }
                />
              </div>
              <div className="flex items-start justify-between gap-4">
                <div className="space-y-1">
                  <Label htmlFor="origin-response-buffering">
                    {t("源响应缓冲区")}
                  </Label>
                  <p className="text-xs leading-5 text-muted-foreground">
                    {t("关闭后普通响应直接透传；流式响应始终直传")}
                  </p>
                </div>
                <Switch
                  id="origin-response-buffering"
                  className="mt-0.5 shrink-0"
                  checked={
                    draft.origin_response_buffering && !draft.passthrough
                  }
                  disabled={draft.passthrough}
                  onCheckedChange={(origin_response_buffering) =>
                    setDraft({
                      ...draft,
                      origin_response_buffering,
                    })
                  }
                />
              </div>
            </div>
            <Separator />
            <div className="flex items-start justify-between gap-4">
              <div className="space-y-1">
                <Label htmlFor="dynamic-compression">{t("动态压缩")}</Label>
                <p className="text-xs leading-5 text-muted-foreground">
                  Gzip / Brotli / Zstandard
                </p>
              </div>
              <Switch
                id="dynamic-compression"
                className="mt-0.5 shrink-0"
                checked={draft.dynamic_compression_enabled}
                onCheckedChange={(dynamic_compression_enabled) =>
                  setDraft({
                    ...draft,
                    dynamic_compression_enabled,
                  })
                }
              />
            </div>
            <div className="grid gap-2">
              <Label htmlFor="compression-excluded-mime-types">
                {t("不压缩的 MIME 类型")}
              </Label>
              <Textarea
                id="compression-excluded-mime-types"
                rows={3}
                value={draft.compression_excluded_mime_types}
                onChange={(event) =>
                  setDraft({
                    ...draft,
                    compression_excluded_mime_types: event.target.value,
                  })
                }
                placeholder="text/event-stream"
              />
            </div>
            <div className="flex items-center justify-between">
              <div>
                <Label htmlFor="passthrough">{t("回源直通")}</Label>
                <p className="text-xs text-muted-foreground">
                  {t("关闭缓存并直接代理到源站")}
                </p>
              </div>
              <Switch
                id="passthrough"
                checked={draft.passthrough}
                onCheckedChange={(passthrough) =>
                  setDraft({
                    ...draft,
                    passthrough,
                  })
                }
              />
            </div>
            <div className="grid gap-4 sm:grid-cols-[1fr_10rem]">
              <div className="flex items-center justify-between">
                <div>
                  <Label htmlFor="inherit-ttl">{t("继承全局 DNS TTL")}</Label>
                  <p className="text-xs text-muted-foreground">
                    {t("范围 60–300 秒")}
                  </p>
                </div>
                <Switch
                  id="inherit-ttl"
                  checked={draft.inherit_dns_ttl}
                  onCheckedChange={(inherit_dns_ttl) =>
                    setDraft({
                      ...draft,
                      inherit_dns_ttl,
                    })
                  }
                />
              </div>
              <Input
                type="number"
                min={60}
                max={300}
                disabled={draft.inherit_dns_ttl}
                value={draft.dns_ttl_seconds}
                onChange={(event) =>
                  setDraft({
                    ...draft,
                    dns_ttl_seconds: Number(event.target.value),
                  })
                }
                aria-label={t("站点 DNS TTL")}
              />
            </div>
          </TabsContent>
          <TabsContent value="tcp" className="mt-5">
            <Alert>
              <Network />
              <AlertTitle>{t("纯 TCP/TLS 模式")}</AlertTitle>
              <AlertDescription>
                {t(
                  "此模式不创建 HTTP 入口，请至少在下方配置一个 TCP 转发端口。",
                )}
              </AlertDescription>
            </Alert>
          </TabsContent>
        </Tabs>
      </CardContent>
    </Card>
  );
}
function OriginFields({
  title,
  required = false,
  url,
  host,
  sni,
  httpVersion,
  healthMethod,
  healthPath,
  wireGuardTunnelID,
  tunnels,
  nodeIDs,
  onChange,
}: {
  title: string;
  required?: boolean;
  url: string;
  host: string;
  sni: string;
  httpVersion: OriginHTTPVersion;
  healthMethod: OriginHealthCheckMethod;
  healthPath: string;
  wireGuardTunnelID: string;
  tunnels: WireGuardTunnel[];
  nodeIDs: string[];
  onChange: (values: {
    url: string;
    host: string;
    sni: string;
    httpVersion: OriginHTTPVersion;
    healthMethod: OriginHealthCheckMethod;
    healthPath: string;
    wireGuardTunnelID: string;
  }) => void;
}) {
  const tls = /^(https|wss|grpcs):/i.test(url);
  const supportsHTTPVersion = /^https?:/i.test(url);
  const supportsHealthCheck = /^(https?|wss?):/i.test(url);
  const selectableTunnels = tunnels.filter(
    (tunnel) =>
      tunnel.id === wireGuardTunnelID ||
      nodeIDs.every((nodeID) =>
        tunnel.peers.some((peer) => peer.node_id === nodeID),
      ),
  );
  const update = (
    values: Partial<{
      url: string;
      host: string;
      sni: string;
      httpVersion: OriginHTTPVersion;
      healthMethod: OriginHealthCheckMethod;
      healthPath: string;
      wireGuardTunnelID: string;
    }>,
  ) =>
    onChange({
      url,
      host,
      sni,
      httpVersion,
      healthMethod,
      healthPath,
      wireGuardTunnelID,
      ...values,
    });
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <div className="sm:col-span-2 text-sm font-medium">{title}</div>
      <Field label={t("源站 URL")} id={`${title}-url`}>
        <Input
          id={`${title}-url`}
          required={required}
          value={url}
          onChange={(event) =>
            update({
              url: event.target.value,
              httpVersion: compatibleOriginHTTPVersion(
                event.target.value,
                httpVersion,
              ),
            })
          }
          placeholder="https://origin.example.com:443"
        />
      </Field>
      <Field label="Host Header" id={`${title}-host`}>
        <Input
          id={`${title}-host`}
          value={host}
          onChange={(event) => update({ host: event.target.value })}
          placeholder="origin.example.com"
        />
      </Field>
      <SelectField
        label={t("回源链路")}
        value={wireGuardTunnelID || "direct"}
        onChange={(value) =>
          update({ wireGuardTunnelID: value === "direct" ? "" : value })
        }
        options={[
          ["direct", t("公网直连")],
          ...selectableTunnels.map((tunnel) => [
            tunnel.id,
            `${t("隧道")} · ${tunnel.name}`,
          ]),
        ]}
      />
      {supportsHTTPVersion ? (
        <SelectField
          label={t("回源 HTTP 协议")}
          value={httpVersion}
          onChange={(value) =>
            update({ httpVersion: value as OriginHTTPVersion })
          }
          options={originHTTPVersionOptions(url)}
        />
      ) : null}
      {supportsHealthCheck ? (
        <>
          <SelectField
            label={t("健康检查方式")}
            value={healthMethod}
            onChange={(value) =>
              update({ healthMethod: value as OriginHealthCheckMethod })
            }
            options={[
              ["HEAD", "HEAD"],
              ["GET", "GET"],
            ]}
          />
          <Field label={t("健康检查路径")} id={`${title}-health-path`}>
            <Input
              id={`${title}-health-path`}
              value={healthPath}
              onChange={(event) => update({ healthPath: event.target.value })}
              placeholder="/"
            />
          </Field>
        </>
      ) : null}
      {tls ? (
        <div className="grid gap-2 sm:col-span-2">
          <Label htmlFor={`${title}-sni`}>{t("回源 TLS SNI")}</Label>
          <Input
            id={`${title}-sni`}
            value={sni}
            onChange={(event) => update({ sni: event.target.value })}
            placeholder="origin.example.com"
          />
        </div>
      ) : null}
    </div>
  );
}
function NodeSelector({
  nodes,
  primarySelected,
  backupSelected,
  onChange,
}: {
  nodes: Node[];
  primarySelected: string[];
  backupSelected: string[];
  onChange: (primarySelected: string[], backupSelected: string[]) => void;
}) {
  const available = nodes.filter(
    (node) => !["revoked", "uninstalling", "uninstalled"].includes(node.status),
  );
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("边缘节点")}</CardTitle>
        <CardDescription>
          {t("主节点不可用时自动启用备用节点调度")}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-5">
        {available.length ? (
          <>
            <NodeChecklist
              title={t("主节点")}
              description={t("正常情况下参与流量调度")}
              nodes={available}
              selected={primarySelected}
              onToggle={(nodeID, checked) =>
                onChange(
                  checked
                    ? [...primarySelected, nodeID]
                    : primarySelected.filter((id) => id !== nodeID),
                  checked
                    ? backupSelected.filter((id) => id !== nodeID)
                    : backupSelected,
                )
              }
            />
            <Separator />
            <NodeChecklist
              title={t("备用节点")}
              description={t("仅在所有主节点不可用时参与流量调度")}
              nodes={available}
              selected={backupSelected}
              onToggle={(nodeID, checked) =>
                onChange(
                  checked
                    ? primarySelected.filter((id) => id !== nodeID)
                    : primarySelected,
                  checked
                    ? [...backupSelected, nodeID]
                    : backupSelected.filter((id) => id !== nodeID),
                )
              }
            />
          </>
        ) : (
          <EmptyState
            title={t("没有可用节点")}
            description={t("先添加边缘节点或恢复节点授权")}
          />
        )}
      </CardContent>
    </Card>
  );
}

function NodeChecklist({
  title,
  description,
  nodes,
  selected,
  onToggle,
}: {
  title: string;
  description: string;
  nodes: Node[];
  selected: string[];
  onToggle: (nodeID: string, checked: boolean) => void;
}) {
  const pagination = useListPagination(nodes);
  return (
    <section>
      <div className="mb-3">
        <h3 className="text-sm font-medium">{title}</h3>
        <p className="text-xs text-muted-foreground">{description}</p>
      </div>
      <div className="grid gap-2 sm:grid-cols-2">
        {pagination.items.map((node) => {
          const checked = selected.includes(node.id);
          return (
            <label
              key={node.id}
              className="flex items-center gap-3 rounded-lg border px-3 py-3 text-sm hover:bg-muted/30"
            >
              <Checkbox
                checked={checked}
                onCheckedChange={(value) => onToggle(node.id, value === true)}
              />
              <span className="min-w-0 flex-1">
                <span className="block truncate font-medium">{node.name}</span>
                <span className="block font-mono text-xs text-muted-foreground">
                  {node.public_ipv4}
                </span>
              </span>
              <StatusBadge status={node.status} />
            </label>
          );
        })}
      </div>
      <ListPagination
        pagination={pagination}
        itemLabel={t("个节点")}
        className="mt-3 rounded-lg border"
      />
    </section>
  );
}
function TCPForwards({
  draft,
  setDraft,
}: {
  draft: SiteDraft;
  setDraft: (draft: SiteDraft) => void;
}) {
  const pagination = useListPagination(draft.tcp_forwards);
  const update = (index: number, values: Partial<TCPForward>) =>
    setDraft({
      ...draft,
      tcp_forwards: draft.tcp_forwards.map((item, current) =>
        current === index
          ? {
              ...item,
              ...values,
            }
          : item,
      ),
    });
  return (
    <Card>
      <CardHeader className="flex-row items-start justify-between gap-4">
        <div>
          <CardTitle>{t("TCP / TLS 转发")}</CardTitle>
          <CardDescription>
            {t("可与 HTTP 入口同时使用，最多 32 个端口")}
          </CardDescription>
        </div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={draft.tcp_forwards.length >= 32}
          onClick={() =>
            setDraft({
              ...draft,
              tcp_forwards: [...draft.tcp_forwards, emptyForward()],
            })
          }
        >
          <Plus />
          {t("添加端口")}
        </Button>
      </CardHeader>
      <CardContent className="space-y-3">
        {draft.tcp_forwards.length ? (
          <>
            {pagination.items.map((forward, pageIndex) => {
              const index = pagination.startIndex + pageIndex;
              return (
                <div key={index} className="relative rounded-lg border p-4">
                  <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
                    <Field label={t("名称")} id={`tcp-name-${index}`}>
                      <Input
                        id={`tcp-name-${index}`}
                        required
                        maxLength={100}
                        value={forward.name}
                        onChange={(event) =>
                          update(index, {
                            name: event.target.value,
                          })
                        }
                        placeholder="IMAPS"
                      />
                    </Field>
                    <Field label={t("监听端口")} id={`tcp-listen-${index}`}>
                      <Input
                        id={`tcp-listen-${index}`}
                        required
                        type="number"
                        min={1}
                        max={65535}
                        value={forward.listen_port || ""}
                        onChange={(event) =>
                          update(index, {
                            listen_port: Number(event.target.value),
                          })
                        }
                      />
                    </Field>
                    <Field label={t("上游主机")} id={`tcp-host-${index}`}>
                      <Input
                        id={`tcp-host-${index}`}
                        required
                        value={forward.upstream_host}
                        onChange={(event) =>
                          update(index, {
                            upstream_host: event.target.value,
                          })
                        }
                      />
                    </Field>
                    <Field label={t("上游端口")} id={`tcp-upstream-${index}`}>
                      <Input
                        id={`tcp-upstream-${index}`}
                        required
                        type="number"
                        min={1}
                        max={65535}
                        value={forward.upstream_port || ""}
                        onChange={(event) =>
                          update(index, {
                            upstream_port: Number(event.target.value),
                          })
                        }
                      />
                    </Field>
                    <SelectField
                      label={t("连接超时")}
                      value={String(forward.connect_timeout_seconds)}
                      onChange={(value) =>
                        update(index, {
                          connect_timeout_seconds: Number(value),
                        })
                      }
                      options={[
                        ["5", t("5 秒")],
                        ["10", t("10 秒")],
                        ["30", t("30 秒")],
                        ["60", t("60 秒")],
                      ]}
                    />
                    <SelectField
                      label={t("空闲超时")}
                      value={String(forward.idle_timeout_seconds)}
                      onChange={(value) =>
                        update(index, {
                          idle_timeout_seconds: Number(value),
                        })
                      }
                      options={[
                        ["300", t("5 分钟")],
                        ["900", t("15 分钟")],
                        ["1800", t("30 分钟")],
                        ["3600", t("60 分钟")],
                      ]}
                    />
                    <Toggle
                      label={t("入口 TLS")}
                      checked={forward.listen_tls}
                      onChange={(listen_tls) =>
                        update(index, {
                          listen_tls,
                        })
                      }
                    />
                    <Toggle
                      label={t("上游 TLS")}
                      checked={forward.upstream_tls}
                      onChange={(upstream_tls) =>
                        update(index, {
                          upstream_tls,
                        })
                      }
                    />
                    {forward.upstream_tls ? (
                      <div className="grid gap-2 sm:col-span-2 xl:col-span-4">
                        <Label htmlFor={`tcp-sni-${index}`}>
                          {t("上游 TLS SNI")}
                        </Label>
                        <Input
                          id={`tcp-sni-${index}`}
                          value={forward.upstream_tls_server_name || ""}
                          onChange={(event) =>
                            update(index, {
                              upstream_tls_server_name: event.target.value,
                            })
                          }
                        />
                      </div>
                    ) : null}
                  </div>
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon-sm"
                    className="absolute right-2 top-2"
                    aria-label={t("删除 TCP 转发")}
                    onClick={() =>
                      setDraft({
                        ...draft,
                        tcp_forwards: draft.tcp_forwards.filter(
                          (_, current) => current !== index,
                        ),
                      })
                    }
                  >
                    <X />
                  </Button>
                </div>
              );
            })}
            <ListPagination
              pagination={pagination}
              itemLabel={t("个转发端口")}
              className="rounded-lg border"
            />
          </>
        ) : (
          <EmptyState
            title={t("未配置 TCP 转发")}
            description={
              draft.tcp_only
                ? t("纯 TCP 模式至少需要一个监听端口")
                : t("可选：为站点增加四层转发端口")
            }
          />
        )}
      </CardContent>
    </Card>
  );
}

function SiteOperations({
  site,
  nodes,
  tls,
  publish,
  deletion,
  pending,
  onPublish,
  onCertificate,
  onManageCache,
  onAllowlist,
  onDelete,
}: {
  site: Site;
  nodes: Node[];
  tls?: TLSStatus;
  publish?: PublishStatus;
  deletion?: PublishStatus;
  pending: boolean;
  onPublish: () => void;
  onCertificate: () => void;
  onManageCache: () => void;
  onAllowlist: () => void;
  onDelete: () => void;
}) {
  const publishTask = site.deleting ? deletion?.task : publish?.task;
  const operationNodes = site.deleting ? deletion?.nodes : publish?.nodes;
  const publishActive = activeTask(publishTask);
  const certActive = activeTask(tls?.certificate_task);
  const publishTaskCurrent = taskMatchesCurrentSite(publishTask, site);
  const visiblePublishTask = publishTaskCurrent ? publishTask : undefined;
  const nodeByID = new Map(nodes.map((node) => [node.id, node]));
  const backupNodeIDs = site.backup_node_ids ?? [];
  const assignedNodeIDs = [...site.node_ids, ...backupNodeIDs];
  const primaryNodeIDs = new Set(site.node_ids);
  const assignedNodes = assignedNodeIDs.map((nodeID) => {
    const node = nodeByID.get(nodeID);
    return {
      id: nodeID,
      name: node?.name || nodeID,
      publicIPv4: node?.public_ipv4,
      status: node?.status,
      role: primaryNodeIDs.has(nodeID) ? t("主节点") : t("备用节点"),
    };
  });
  const assignedPagination = useListPagination(assignedNodes);
  const publishPagination = useListPagination(operationNodes ?? []);
  const showPublishTargets = Boolean(
    publishTaskCurrent &&
    operationNodes?.length &&
    (publishActive ||
      ["partial", "failed"].includes(publishTask?.status ?? "")),
  );
  const cacheable =
    !site.tcp_only &&
    !site.passthrough &&
    site.origin_response_buffering !== false &&
    /^https?:/i.test(site.primary_origin.url);
  const needsTLS = siteNeedsCertificate(site);
  return (
    <Card>
      <CardHeader>
        <CardTitle>{t("发布与运维")}</CardTitle>
        <CardDescription>{t("配置保存后需发布到边缘节点")}</CardDescription>
      </CardHeader>
      <CardContent className="grid gap-3">
        <OperationState
          label={t("发布")}
          task={visiblePublishTask}
          fallback={
            site.published
              ? t("已发布")
              : t("有未发布更改，目标 {value0} 个节点", {
                  value0: assignedNodeIDs.length,
                })
          }
          detail={
            site.published && visiblePublishTask?.status === "succeeded"
              ? t("当前配置已发布到 {value0} 个边缘节点", {
                  value0: assignedNodeIDs.length,
                })
              : undefined
          }
        />
        {needsTLS ? (
          <OperationState
            label="TLS"
            task={tls?.certificate_task}
            fallback={t("尚未签发")}
            extra={tls?.published_after_certificate ? t("已部署") : undefined}
          />
        ) : null}
        <div className="rounded-lg border px-3 py-2">
          <div className="flex items-center justify-between gap-3">
            <div className="min-w-0">
              <div className="text-sm">{t("节点分配")}</div>
              <div className="text-xs text-muted-foreground">
                {site.published ? t("当前承载节点") : t("待发布节点")} ·{" "}
                {t("{value0} 个主节点，{value1} 个备用节点", {
                  value0: site.node_ids.length,
                  value1: backupNodeIDs.length,
                })}
              </div>
            </div>
            <StatusBadge
              status={site.published ? "succeeded" : "pending"}
              label={site.published ? t("已发布") : t("待发布")}
            />
          </div>
          {assignedNodes.length ? (
            <>
              <div className="mt-3 max-h-44 overflow-auto rounded-lg border">
                <Table>
                  <TableBody>
                    {assignedPagination.items.map((node) => (
                      <TableRow key={node.id}>
                        <TableCell className="text-xs">
                          <span className="block">{node.name}</span>
                          {node.publicIPv4 ? (
                            <span className="font-mono text-muted-foreground">
                              {node.publicIPv4}
                            </span>
                          ) : null}
                          <span className="block text-muted-foreground">
                            {node.role}
                          </span>
                        </TableCell>
                        <TableCell className="text-right">
                          {node.status ? (
                            <StatusBadge status={node.status} />
                          ) : (
                            <span className="text-xs text-muted-foreground">
                              {t("信息缺失")}
                            </span>
                          )}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
              {assignedNodes.length > 20 ? (
                <ListPagination
                  pagination={assignedPagination}
                  itemLabel={t("个节点")}
                  className="border-x border-b"
                />
              ) : null}
            </>
          ) : (
            <p className="mt-3 text-xs text-muted-foreground">
              {t("当前配置未分配边缘节点")}
            </p>
          )}
        </div>
        <Button
          type="button"
          disabled={site.deleting || pending || publishActive}
          onClick={onPublish}
        >
          <Rocket />
          {site.published ? t("重新发布") : t("发布站点")}
        </Button>
        {needsTLS && tls && !certActive ? (
          <Button
            type="button"
            variant="outline"
            disabled={site.deleting || pending}
            onClick={onCertificate}
          >
            <KeyRound />
            {tls.certificate_task ? t("重新申请 TLS") : t("申请 TLS")}
          </Button>
        ) : null}
        {cacheable ? (
          <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border px-3 py-2">
            <div className="min-w-0">
              <div className="text-sm">{t("缓存版本")}</div>
              <div className="text-xs text-muted-foreground">
                Cache Version V{formatNumber(site.cache_generation)}
              </div>
            </div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              disabled={site.deleting}
              onClick={onManageCache}
            >
              <ArrowRight />
              {t("打开缓存运维台")}
            </Button>
          </div>
        ) : null}
        <Button
          type="button"
          variant="outline"
          disabled={site.deleting}
          onClick={onAllowlist}
        >
          <ShieldCheck />
          {t("源站白名单")}
        </Button>
        {showPublishTargets ? (
          <div className="overflow-hidden rounded-lg border">
            <div className="border-b px-3 py-2 text-xs text-muted-foreground">
              {site.deleting ? t("本次删除涉及节点") : t("本次发布涉及节点")}
            </div>
            <div className="max-h-44 overflow-auto">
              <Table>
                <TableBody>
                  {publishPagination.items.map((node) => (
                    <TableRow key={node.node_id}>
                      <TableCell className="text-xs">
                        {node.node_name || node.node_id}
                      </TableCell>
                      <TableCell className="text-right">
                        <StatusBadge status={node.status} />
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
            <ListPagination
              pagination={publishPagination}
              itemLabel={t("个节点")}
            />
          </div>
        ) : null}
        <Separator />
        <Button
          type="button"
          variant="destructive"
          disabled={pending || publishActive || certActive}
          onClick={onDelete}
        >
          <Trash2 />
          {site.deleting ? t("查看/重试删除") : t("删除站点")}
        </Button>
      </CardContent>
    </Card>
  );
}
function AllowlistDialog({
  open,
  onOpenChange,
  data,
  loading,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  data?: Allowlist;
  loading: boolean;
}) {
  const entries = data?.nodes?.length
    ? data.nodes
    : (data?.ipv4_cidrs ?? []).map((cidr, index) => ({
        node_id: `legacy-${index}`,
        node_name: t("边缘节点"),
        ipv4_cidr: cidr,
        assignment: "current" as const,
      }));
  const currentNodes = entries.filter(
    (node) => node.assignment !== "published_only",
  );
  const pendingRemovalNodes = entries.filter(
    (node) => node.assignment === "published_only",
  );
  const currentPagination = useListPagination(currentNodes);
  const pendingRemovalPagination = useListPagination(pendingRemovalNodes);
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("源站防火墙白名单")}</DialogTitle>
          <DialogDescription>
            {t("当前配置与发布过渡期的节点地址")}
          </DialogDescription>
        </DialogHeader>
        {loading ? (
          <div className="py-8 text-center text-sm text-muted-foreground">
            {t("正在加载...")}
          </div>
        ) : (
          <div className="space-y-3">
            {entries.length ? (
              <>
                <AllowlistNodeGroup
                  title={t("当前配置节点")}
                  nodes={currentPagination.items}
                  assignmentLabel={t("当前配置")}
                  assignmentStatus="succeeded"
                />
                {currentNodes.length > 20 ? (
                  <ListPagination
                    pagination={currentPagination}
                    itemLabel={t("个节点")}
                    className="rounded-lg border"
                  />
                ) : null}
                {pendingRemovalNodes.length ? (
                  <>
                    <AllowlistNodeGroup
                      title={t("发布后移除")}
                      nodes={pendingRemovalPagination.items}
                      assignmentLabel={t("过渡期保留")}
                      assignmentStatus="pending"
                    />
                    {pendingRemovalNodes.length > 20 ? (
                      <ListPagination
                        pagination={pendingRemovalPagination}
                        itemLabel={t("个节点")}
                        className="rounded-lg border"
                      />
                    ) : null}
                  </>
                ) : null}
              </>
            ) : (
              <EmptyState title={t("暂无可用地址")} />
            )}
            <p className="text-xs leading-5 text-muted-foreground">
              {data?.note}
            </p>
          </div>
        )}
        <DialogFooter>
          <Button onClick={() => onOpenChange(false)}>{t("完成")}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
function AllowlistNodeGroup({
  title,
  nodes,
  assignmentLabel,
  assignmentStatus,
}: {
  title: string;
  nodes: NonNullable<Allowlist["nodes"]>;
  assignmentLabel: string;
  assignmentStatus: string;
}) {
  if (!nodes.length) return null;
  return (
    <div className="space-y-2">
      <div className="text-xs font-medium text-muted-foreground">{title}</div>
      {nodes.map((node) => (
        <div
          key={node.node_id}
          className="flex flex-wrap items-center gap-2 rounded-lg border px-3 py-2"
        >
          <div className="min-w-40 flex-1">
            <div className="truncate text-sm font-medium">{node.node_name}</div>
            <code className="text-xs text-muted-foreground">
              {node.ipv4_cidr}
            </code>
          </div>
          <StatusBadge status={assignmentStatus} label={assignmentLabel} />
          <CopyButton value={node.ipv4_cidr} />
        </div>
      ))}
    </div>
  );
}
function OperationState({
  label,
  task,
  fallback,
  extra,
  detail,
}: {
  label: string;
  task?: DeploymentTask | null;
  fallback: string;
  extra?: string;
  detail?: string;
}) {
  const visibleDetail = detail || localizeTaskDetail(task?.detail);
  return (
    <div className="flex items-start justify-between gap-3 rounded-lg border px-3 py-2 text-sm">
      <div>
        <span>{label}</span>
        {visibleDetail ? (
          <p className="mt-1 text-xs text-muted-foreground">{visibleDetail}</p>
        ) : null}
      </div>
      {task ? (
        <StatusBadge status={task.status} />
      ) : (
        <span className="text-xs text-muted-foreground">
          {extra || fallback}
        </span>
      )}
    </div>
  );
}
function Fact({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center justify-between gap-3">
      <span className="text-muted-foreground">{label}</span>
      <span className="text-right font-medium">{value}</span>
    </div>
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
function SelectField({
  label,
  value,
  onChange,
  options,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  options: string[][];
}) {
  const id = useId();
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{label}</Label>
      <Select value={value} onValueChange={onChange}>
        <SelectTrigger id={id} className="w-full">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {options.map(([key, text]) => (
            <SelectItem key={key} value={key}>
              {text}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}
function Toggle({
  label,
  checked,
  onChange,
}: {
  label: string;
  checked: boolean;
  onChange: (checked: boolean) => void;
}) {
  const id = useId();
  return (
    <div className="flex items-center justify-between rounded-lg border px-3 py-2">
      <Label htmlFor={id}>{label}</Label>
      <Switch id={id} checked={checked} onCheckedChange={onChange} />
    </div>
  );
}
function siteNeedsCertificate(site: Site) {
  return (
    !site.tcp_only || site.tcp_forwards.some((forward) => forward.listen_tls)
  );
}
function localizeTaskDetail(detail?: string) {
  if (!detail) return undefined;
  const exact: Record<string, string> = {
    "building node configurations": t("正在生成边缘节点配置"),
    "preparing edge configuration confirmation": t("正在等待边缘节点确认配置"),
    "configuration staged; no active assigned edge nodes to confirm": t(
      "配置已暂存，当前没有需要确认的在线分配节点",
    ),
    "publish task did not create edge confirmation targets; retry Publish": t(
      "发布任务未生成边缘确认目标，请重试发布",
    ),
    "queued for DNS-01 certificate issuance": t("TLS 证书签发已排队"),
    "queued for certificate renewal": t("TLS 证书续期已排队"),
    "preparing DNS-01 certificate issuance": t("正在准备 DNS-01 证书签发"),
    "waiting for DNS-01 validation": t("正在等待 DNS-01 验证"),
    "certificate renewed": t("TLS 证书已续期"),
    "certificate stored; publish the site to deploy it": t(
      "TLS 证书已保存，请发布站点以部署到边缘节点",
    ),
    "certificate queue is full; retry Issue TLS": t(
      "证书任务队列已满，请重试 TLS 签发",
    ),
    "certificate issuance interrupted by control-plane shutdown; retry Issue TLS":
      t("控制面停止导致证书签发中断，请重试 TLS 签发"),
  };
  if (exact[detail]) return exact[detail];
  let match = detail.match(
    /^configuration applied by (\d+) active edge node\(s\)$/,
  );
  if (match)
    return t("配置变更已由 {value0} 个受影响的在线边缘节点应用", {
      value0: match[1],
    });
  match = detail.match(
    /^configuration applied by (\d+) of (\d+) active edge node\(s\)$/,
  );
  if (match)
    return t("配置变更已由 {value0}/{value1} 个受影响的在线边缘节点应用", {
      value0: match[1],
      value1: match[2],
    });
  match = detail.match(
    /^(\d+) edge node\(s\) did not apply the configuration$/,
  );
  if (match)
    return t("{value0} 个受影响的边缘节点未能应用配置", {
      value0: match[1],
    });
  return detail;
}
function emptyDraft(ttl: number): SiteDraft {
  return {
    name: "",
    domains: "",
    node_ids: [],
    backup_node_ids: [],
    primary_url: "https://",
    primary_host: "",
    primary_sni: "",
    primary_http_version: "http1",
    primary_health_method: "HEAD",
    primary_health_path: "/",
    primary_wireguard_tunnel_id: "",
    backup_enabled: false,
    backup_url: "",
    backup_host: "",
    backup_sni: "",
    backup_http_version: "http1",
    backup_health_method: "HEAD",
    backup_health_path: "/",
    backup_wireguard_tunnel_id: "",
    passthrough: false,
    request_body_buffering: true,
    origin_response_buffering: true,
    dynamic_compression_enabled: true,
    compression_excluded_mime_types: "text/event-stream",
    http3_enabled: false,
    client_max_body_size_mb: 128,
    client_keepalive_timeout_seconds: 120,
    read_write_timeout_seconds: 120,
    inherit_dns_ttl: true,
    dns_ttl_seconds: ttl,
    tcp_only: false,
    tcp_forwards: [],
    enabled: true,
  };
}
function emptyForward(): TCPForward {
  return {
    name: "",
    listen_port: 0,
    listen_tls: true,
    upstream_host: "",
    upstream_port: 0,
    upstream_tls: true,
    upstream_tls_server_name: "",
    connect_timeout_seconds: 10,
    idle_timeout_seconds: 300,
  };
}
function draftFromSite(site: Site, ttl: number): SiteDraft {
  return {
    name: site.name,
    domains: site.domains.join(", "),
    node_ids: [...site.node_ids],
    backup_node_ids: [...(site.backup_node_ids ?? [])],
    primary_url: site.primary_origin.url,
    primary_host: site.primary_origin.host_header || "",
    primary_sni: site.primary_origin.tls_server_name || "",
    primary_http_version: site.primary_origin.http_version || "http1",
    primary_health_method: site.primary_origin.health_check_method || "HEAD",
    primary_health_path: site.primary_origin.health_check_path || "/",
    primary_wireguard_tunnel_id: site.primary_origin.wireguard_tunnel_id || "",
    backup_enabled: Boolean(site.backup_origin),
    backup_url: site.backup_origin?.url || "",
    backup_host: site.backup_origin?.host_header || "",
    backup_sni: site.backup_origin?.tls_server_name || "",
    backup_http_version: site.backup_origin?.http_version || "http1",
    backup_health_method: site.backup_origin?.health_check_method || "HEAD",
    backup_health_path: site.backup_origin?.health_check_path || "/",
    backup_wireguard_tunnel_id: site.backup_origin?.wireguard_tunnel_id || "",
    passthrough: site.passthrough,
    request_body_buffering: site.request_body_buffering ?? true,
    origin_response_buffering: site.origin_response_buffering ?? true,
    dynamic_compression_enabled: site.dynamic_compression_enabled ?? true,
    compression_excluded_mime_types: (
      site.compression_excluded_mime_types ?? ["text/event-stream"]
    ).join("\n"),
    http3_enabled: site.http3_enabled ?? false,
    client_max_body_size_mb: site.client_max_body_size_mb ?? 128,
    client_keepalive_timeout_seconds:
      site.client_keepalive_timeout_seconds ?? 120,
    read_write_timeout_seconds: site.read_write_timeout_seconds ?? 120,
    inherit_dns_ttl: site.dns_ttl_seconds == null,
    dns_ttl_seconds: site.dns_ttl_seconds ?? ttl,
    tcp_only: site.tcp_only,
    tcp_forwards: site.tcp_forwards.map((forward) => ({
      ...forward,
    })),
    enabled: site.enabled,
  };
}
function splitList(value: string) {
  return value
    .split(/[\n,]/)
    .map((item) => item.trim())
    .filter(Boolean);
}
function sitePayload(draft: SiteDraft) {
  const payload: Record<string, unknown> = {
    name: draft.name,
    domains: splitList(draft.domains),
    node_ids: draft.node_ids,
    backup_node_ids: draft.backup_node_ids,
    primary_origin: {
      url: draft.primary_url,
      host_header: draft.primary_host,
      tls_server_name: /^(https|wss|grpcs):/i.test(draft.primary_url)
        ? draft.primary_sni
        : "",
      http_version: /^(https?|wss?):/i.test(draft.primary_url)
        ? draft.primary_http_version
        : undefined,
      health_check_method: /^(https?|wss?):/i.test(draft.primary_url)
        ? draft.primary_health_method
        : undefined,
      health_check_path: /^(https?|wss?):/i.test(draft.primary_url)
        ? draft.primary_health_path
        : undefined,
      wireguard_tunnel_id: draft.tcp_only
        ? ""
        : draft.primary_wireguard_tunnel_id,
      enabled: true,
    },
    passthrough: draft.passthrough,
    request_body_buffering: draft.request_body_buffering,
    origin_response_buffering: draft.origin_response_buffering,
    dynamic_compression_enabled: draft.dynamic_compression_enabled,
    compression_excluded_mime_types: splitList(
      draft.compression_excluded_mime_types,
    ),
    http3_enabled: draft.tcp_only ? false : draft.http3_enabled,
    client_max_body_size_mb: draft.client_max_body_size_mb,
    client_keepalive_timeout_seconds: draft.client_keepalive_timeout_seconds,
    read_write_timeout_seconds: draft.read_write_timeout_seconds,
    dns_ttl_seconds: draft.inherit_dns_ttl ? null : draft.dns_ttl_seconds,
    tcp_only: draft.tcp_only,
    tcp_forwards: draft.tcp_forwards,
    enabled: draft.enabled,
  };
  if (draft.backup_enabled && draft.backup_url.trim())
    payload.backup_origin = {
      url: draft.backup_url,
      host_header: draft.backup_host,
      tls_server_name: /^(https|wss|grpcs):/i.test(draft.backup_url)
        ? draft.backup_sni
        : "",
      http_version: /^(https?|wss?):/i.test(draft.backup_url)
        ? draft.backup_http_version
        : undefined,
      health_check_method: /^(https?|wss?):/i.test(draft.backup_url)
        ? draft.backup_health_method
        : undefined,
      health_check_path: /^(https?|wss?):/i.test(draft.backup_url)
        ? draft.backup_health_path
        : undefined,
      wireguard_tunnel_id: draft.tcp_only
        ? ""
        : draft.backup_wireguard_tunnel_id,
      enabled: true,
    };
  return payload;
}

function compatibleOriginHTTPVersion(
  url: string,
  current: OriginHTTPVersion,
): OriginHTTPVersion {
  if (/^https:/i.test(url) && (current === "http1" || current === "http2"))
    return current;
  if (/^http:/i.test(url) && (current === "http1" || current === "h2c"))
    return current;
  return "http1";
}

function originHTTPVersionOptions(url: string): string[][] {
  if (/^https:/i.test(url))
    return [
      ["http1", "HTTP/1.1"],
      ["http2", "HTTP/2"],
    ];
  return [
    ["http1", "HTTP/1.1"],
    ["h2c", "HTTP/2 (H2C)"],
  ];
}
