import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  BadgeCheck,
  CalendarClock,
  CircleAlert,
  KeyRound,
  LoaderCircle,
  RefreshCw,
} from "lucide-react";
import { useMemo, useState, type ComponentType } from "react";
import { Link } from "react-router-dom";
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
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useListPagination } from "@/hooks/use-list-pagination";
import { api, errorMessage } from "@/lib/api";
import { formatDate, formatDuration, formatNumber } from "@/lib/format";
import type {
  CertificateOverview,
  CertificateSiteStatus,
  DeploymentTask,
} from "@/lib/types";

type CertificateFilter = "all" | "attention" | "active";

export function CertificatesPage() {
  const queryClient = useQueryClient();
  const [filter, setFilter] = useState<CertificateFilter>("all");
  const query = useQuery({
    queryKey: ["certificates"],
    queryFn: () => api<CertificateOverview>("/api/certificates"),
    refetchInterval: (current) =>
      current.state.data?.sites.some((site) => activeTask(site.task))
        ? 2_000
        : 30_000,
  });
  const now = Date.now();
  const sites = query.data?.sites ?? [];
  const active = sites.filter((site) => activeTask(site.task));
  const attention = sites.filter((site) => needsAttention(site, now));
  const filtered = useMemo(() => {
    if (filter === "active") return active;
    if (filter === "attention") return attention;
    return sites;
  }, [active, attention, filter, sites]);
  const pagination = useListPagination(filtered);
  const mutation = useMutation({
    mutationFn: ({ path }: { site: CertificateSiteStatus; path: string }) =>
      api<DeploymentTask>(path, { method: "POST" }),
    onSuccess: (_, input) => {
      toast.success(
        input.site.certificate_present ? "证书续期已排队" : "证书签发已排队",
      );
      void queryClient.invalidateQueries({ queryKey: ["certificates"] });
      void queryClient.invalidateQueries({
        queryKey: ["site-tls", input.site.site_id],
      });
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  const certificateSites = sites.filter((site) => site.needs_certificate);
  const valid = certificateSites.filter((site) =>
    certificateIsValid(site, now),
  );

  return (
    <>
      <PageHeader
        title="证书"
        description="站点 TLS 证书有效期、部署状态与续期计划"
        actions={
          <Button
            variant="outline"
            size="icon"
            aria-label="刷新证书状态"
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
          sites.length ? (
            <>
              <div className="grid grid-cols-2 gap-px overflow-hidden rounded-lg border bg-border lg:grid-cols-4">
                <Metric
                  icon={KeyRound}
                  label="证书站点"
                  value={certificateSites.length}
                />
                <Metric
                  icon={BadgeCheck}
                  label="有效证书"
                  value={valid.length}
                />
                <Metric
                  icon={CircleAlert}
                  label="需处理"
                  value={attention.length}
                  alert={attention.length > 0}
                />
                <Metric
                  icon={CalendarClock}
                  label="任务进行中"
                  value={active.length}
                />
              </div>

              <Tabs
                value={filter}
                onValueChange={(value) => setFilter(value as CertificateFilter)}
              >
                <TabsList aria-label="证书筛选">
                  <TabsTrigger value="all">全部 {sites.length}</TabsTrigger>
                  <TabsTrigger value="attention">
                    需处理 {attention.length}
                  </TabsTrigger>
                  <TabsTrigger value="active">
                    进行中 {active.length}
                  </TabsTrigger>
                </TabsList>
              </Tabs>

              {filtered.length ? (
                <Panel>
                  <div className="hidden md:block">
                    <Table>
                      <TableHeader>
                        <TableRow>
                          <TableHead className="pl-5">站点</TableHead>
                          <TableHead>证书状态</TableHead>
                          <TableHead>到期时间</TableHead>
                          <TableHead>剩余有效期</TableHead>
                          <TableHead>续期计划</TableHead>
                          <TableHead>部署状态</TableHead>
                          <TableHead className="w-32 pr-5 text-right">
                            操作
                          </TableHead>
                        </TableRow>
                      </TableHeader>
                      <TableBody>
                        {pagination.items.map((site) => (
                          <CertificateRow
                            key={site.site_id}
                            site={site}
                            now={now}
                            pending={
                              mutation.isPending &&
                              mutation.variables?.site.site_id === site.site_id
                            }
                            onAction={(path) => mutation.mutate({ site, path })}
                          />
                        ))}
                      </TableBody>
                    </Table>
                  </div>
                  <div className="divide-y md:hidden">
                    {pagination.items.map((site) => (
                      <CertificateMobileRow
                        key={site.site_id}
                        site={site}
                        now={now}
                        pending={
                          mutation.isPending &&
                          mutation.variables?.site.site_id === site.site_id
                        }
                        onAction={(path) => mutation.mutate({ site, path })}
                      />
                    ))}
                  </div>
                  <ListPagination
                    pagination={pagination}
                    itemLabel="个站点"
                    disabled={query.isFetching}
                  />
                </Panel>
              ) : (
                <EmptyState
                  title="当前筛选无证书记录"
                  description="切换筛选条件查看其他站点"
                />
              )}
              <p className="text-xs text-muted-foreground">
                自动续期在到期前 {query.data.renewal_window_days} 天开始，每{" "}
                {formatDuration(query.data.reconcile_interval_seconds)}{" "}
                检查一次。
              </p>
            </>
          ) : (
            <EmptyState
              title="暂无站点证书"
              description="添加站点后将在这里显示 TLS 证书状态"
            />
          )
        ) : null}
      </PageBody>
    </>
  );
}

function Metric({
  icon: Icon,
  label,
  value,
  alert = false,
}: {
  icon: ComponentType<{ className?: string; "aria-hidden"?: boolean }>;
  label: string;
  value: number;
  alert?: boolean;
}) {
  return (
    <div className="flex min-h-24 items-center gap-3 bg-card px-4 py-4 sm:px-5">
      <div className="flex size-9 shrink-0 items-center justify-center rounded-md border bg-muted/30">
        <Icon
          className={alert ? "size-4 text-destructive" : "size-4 text-primary"}
          aria-hidden={true}
        />
      </div>
      <div className="min-w-0">
        <div className="text-xs text-muted-foreground">{label}</div>
        <div className="mt-0.5 text-xl font-semibold tabular-nums">
          {formatNumber(value)}
        </div>
      </div>
    </div>
  );
}

function CertificateRow({
  site,
  now,
  pending,
  onAction,
}: {
  site: CertificateSiteStatus;
  now: number;
  pending: boolean;
  onAction: (path: string) => void;
}) {
  return (
    <TableRow>
      <TableCell className="pl-5">
        <SiteIdentity site={site} />
      </TableCell>
      <TableCell>
        <CertificateStatus site={site} now={now} />
      </TableCell>
      <TableCell className="whitespace-nowrap text-sm tabular-nums">
        {site.needs_certificate ? formatDate(site.not_after) : "--"}
      </TableCell>
      <TableCell className="whitespace-nowrap text-sm tabular-nums">
        {remainingValidity(site, now)}
      </TableCell>
      <TableCell className="min-w-44 text-sm">
        <RenewalPlan site={site} now={now} />
      </TableCell>
      <TableCell>
        <DeploymentStatus site={site} />
      </TableCell>
      <TableCell className="pr-5 text-right">
        <CertificateAction site={site} pending={pending} onAction={onAction} />
      </TableCell>
    </TableRow>
  );
}

function CertificateMobileRow({
  site,
  now,
  pending,
  onAction,
}: {
  site: CertificateSiteStatus;
  now: number;
  pending: boolean;
  onAction: (path: string) => void;
}) {
  return (
    <div className="space-y-4 p-4">
      <div className="flex items-start justify-between gap-3">
        <SiteIdentity site={site} />
        <CertificateStatus site={site} now={now} />
      </div>
      <dl className="grid grid-cols-2 gap-x-4 gap-y-3 text-sm">
        <MobileFact label="到期时间" value={formatDate(site.not_after)} />
        <MobileFact label="剩余有效期" value={remainingValidity(site, now)} />
        <div className="col-span-2">
          <dt className="text-xs text-muted-foreground">续期计划</dt>
          <dd className="mt-1">
            <RenewalPlan site={site} now={now} />
          </dd>
        </div>
        <div>
          <dt className="text-xs text-muted-foreground">部署状态</dt>
          <dd className="mt-1">
            <DeploymentStatus site={site} />
          </dd>
        </div>
      </dl>
      <CertificateAction
        site={site}
        pending={pending}
        onAction={onAction}
        fullWidth
      />
    </div>
  );
}

function SiteIdentity({ site }: { site: CertificateSiteStatus }) {
  return (
    <div className="min-w-0">
      <Link
        to={`/sites/${encodeURIComponent(site.site_id)}`}
        className="font-medium hover:underline"
      >
        {site.site_name}
      </Link>
      <div className="mt-0.5 max-w-72 truncate text-xs text-muted-foreground">
        {site.domains.join(", ")}
      </div>
    </div>
  );
}

function CertificateStatus({
  site,
  now,
}: {
  site: CertificateSiteStatus;
  now: number;
}) {
  const status = certificateStatus(site, now);
  return (
    <div className="min-w-0">
      <StatusBadge status={status.tone} label={status.label} />
      {site.task?.status === "failed" && site.task.detail ? (
        <div
          className="mt-1 max-w-48 truncate text-xs text-destructive"
          title={site.task.detail}
        >
          {site.task.detail}
        </div>
      ) : null}
    </div>
  );
}

function RenewalPlan({
  site,
  now,
}: {
  site: CertificateSiteStatus;
  now: number;
}) {
  if (!site.needs_certificate)
    return <span className="text-muted-foreground">无需续期</span>;
  if (!site.certificate_present)
    return <span className="text-muted-foreground">签发后生成计划</span>;
  if (activeTask(site.task)) {
    return (
      <span>
        {site.task?.kind === "renew_certificate" ? "正在续期" : "正在签发"}
      </span>
    );
  }
  const dueAt = timestamp(site.renewal_due_at);
  return (
    <div>
      <div>
        {dueAt !== null && dueAt <= now
          ? "已进入续期窗口"
          : formatDate(site.renewal_due_at)}
      </div>
      <div className="mt-0.5 text-xs text-muted-foreground">
        {site.enabled && site.published
          ? "自动续期已启用"
          : "站点未启用或未发布，自动续期暂停"}
      </div>
    </div>
  );
}

function DeploymentStatus({ site }: { site: CertificateSiteStatus }) {
  if (!site.needs_certificate || !site.certificate_present) {
    return <span className="text-sm text-muted-foreground">--</span>;
  }
  return site.published_after_certificate ? (
    <StatusBadge status="succeeded" label="已部署" />
  ) : (
    <StatusBadge status="pending" label="待发布" />
  );
}

function CertificateAction({
  site,
  pending,
  onAction,
  fullWidth = false,
}: {
  site: CertificateSiteStatus;
  pending: boolean;
  onAction: (path: string) => void;
  fullWidth?: boolean;
}) {
  if (!site.needs_certificate) return null;
  const active = activeTask(site.task);
  const encodedID = encodeURIComponent(site.site_id);
  const path = site.certificate_present
    ? `/api/certificates/${encodedID}/renew`
    : `/api/sites/${encodedID}/certificate`;
  return (
    <Button
      variant="outline"
      size="sm"
      className={fullWidth ? "w-full" : undefined}
      disabled={pending || active || site.deleting}
      onClick={() => onAction(path)}
    >
      {pending || active ? (
        <LoaderCircle className="animate-spin" />
      ) : site.certificate_present ? (
        <RefreshCw />
      ) : (
        <KeyRound />
      )}
      {active
        ? site.task?.kind === "renew_certificate"
          ? "续期中"
          : "签发中"
        : site.certificate_present
          ? "手动续期"
          : "签发证书"}
    </Button>
  );
}

function MobileFact({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="mt-1 tabular-nums">{value}</dd>
    </div>
  );
}

function activeTask(task?: DeploymentTask | null) {
  return Boolean(
    task && ["queued", "dispatching", "applying"].includes(task.status),
  );
}

function certificateIsValid(site: CertificateSiteStatus, now: number) {
  const notAfter = timestamp(site.not_after);
  return site.certificate_present && notAfter !== null && notAfter > now;
}

function needsAttention(site: CertificateSiteStatus, now: number) {
  if (!site.needs_certificate) return false;
  if (site.task?.status === "failed" || !site.certificate_present) return true;
  const notAfter = timestamp(site.not_after);
  const dueAt = timestamp(site.renewal_due_at);
  return (
    notAfter === null || notAfter <= now || (dueAt !== null && dueAt <= now)
  );
}

function certificateStatus(site: CertificateSiteStatus, now: number) {
  if (!site.needs_certificate) return { tone: "ready", label: "无需证书" };
  if (activeTask(site.task)) {
    return {
      tone: site.task?.status ?? "applying",
      label: site.task?.kind === "renew_certificate" ? "续期中" : "签发中",
    };
  }
  if (site.task?.status === "failed")
    return { tone: "failed", label: "任务失败" };
  if (!site.certificate_present) return { tone: "pending", label: "未签发" };
  const notAfter = timestamp(site.not_after);
  if (notAfter === null) return { tone: "failed", label: "到期日未知" };
  if (notAfter <= now) return { tone: "failed", label: "已过期" };
  const dueAt = timestamp(site.renewal_due_at);
  if (dueAt !== null && dueAt <= now)
    return { tone: "pending", label: "待续期" };
  return { tone: "succeeded", label: "有效" };
}

function remainingValidity(site: CertificateSiteStatus, now: number) {
  if (!site.needs_certificate || !site.certificate_present) return "--";
  const notAfter = timestamp(site.not_after);
  if (notAfter === null) return "未知";
  const seconds = Math.floor((notAfter - now) / 1_000);
  if (seconds <= 0) return `已过期 ${formatDuration(Math.abs(seconds))}`;
  return formatDuration(seconds);
}

function timestamp(value?: string) {
  if (!value) return null;
  const parsed = new Date(value).getTime();
  return Number.isNaN(parsed) ? null : parsed;
}
