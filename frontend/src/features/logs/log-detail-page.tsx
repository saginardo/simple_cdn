import { useQuery } from "@tanstack/react-query";
import {
  ArrowDownToLine,
  ArrowLeft,
  ArrowUpFromLine,
  Clock3,
  Globe2,
  Server,
} from "lucide-react";
import type { ReactNode } from "react";
import { useNavigate, useParams } from "react-router";
import { CopyButton } from "@/components/copy-button";
import { HTTPStatusBadge } from "@/components/http-status-badge";
import {
  EmptyState,
  PageBody,
  PageError,
  PageHeader,
  PageLoading,
} from "@/components/page";
import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { api } from "@/lib/api";
import { formatBytes, formatDateTime, formatNumber } from "@/lib/format";
import type { AccessLog, Node, Site } from "@/lib/types";
import { t, useI18n } from "@/lib/i18n";
export function LogDetailPage() {
  useI18n();
  const { logId = "" } = useParams();
  const navigate = useNavigate();
  const encodedID = encodeURIComponent(logId);
  const log = useQuery({
    queryKey: ["log", logId],
    queryFn: () => api<AccessLog>(`/api/logs/${encodedID}`),
    enabled: Boolean(logId),
    retry: false,
  });
  const sites = useQuery({
    queryKey: ["sites"],
    queryFn: () => api<Site[]>("/api/sites"),
  });
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => api<Node[]>("/api/nodes"),
  });
  return (
    <>
      <PageHeader
        title={t("请求详情")}
        description={log.data ? formatDateTime(log.data.timestamp) : undefined}
        actions={
          <Button variant="outline" onClick={() => navigate("/logs")}>
            <ArrowLeft />
            {t("返回日志")}
          </Button>
        }
      />
      <PageBody>
        {log.isLoading ? <PageLoading rows={3} /> : null}
        {log.error ? (
          <PageError title={t("请求详情加载失败")} error={log.error} />
        ) : null}
        {log.data ? (
          <LogDetails
            entry={log.data}
            siteName={nameFor(sites.data, log.data.site_id)}
            nodeName={nameFor(nodes.data, log.data.node_id)}
          />
        ) : null}
        {!logId ? <EmptyState title={t("缺少日志 ID")} /> : null}
      </PageBody>
    </>
  );
}
function LogDetails({
  entry,
  siteName,
  nodeName,
}: {
  entry: AccessLog;
  siteName: string;
  nodeName: string;
}) {
  const headers = [
    ["Host", entry.host],
    ["User-Agent", entry.user_agent],
    ["Referer", entry.referer],
    ["Content-Type", entry.content_type],
    ["Accept", entry.accept],
    ["Range", entry.range],
  ].filter((item): item is [string, string] => Boolean(item[1]));
  const completion = completionState(entry.request_completion);
  return (
    <div className="space-y-5">
      <Card>
        <CardHeader>
          <div className="flex flex-wrap items-center gap-2">
            <span className="font-mono text-sm font-semibold">
              {entry.method}
            </span>
            <HTTPStatusBadge status={entry.status} />
            <StatusBadge status={entry.cache_status || "UNCACHED"} />
          </div>
          <CardTitle className="break-all font-mono text-base leading-6">
            {entry.path}
          </CardTitle>
          <CardDescription className="flex min-w-0 items-center gap-2">
            <span className="truncate">
              {t("请求 ID：")}
              {entry.id}
            </span>
            <CopyButton value={entry.id} label={t("复制请求 ID")} />
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-px border-t bg-border sm:grid-cols-2 xl:grid-cols-4">
          <Metric
            icon={<Globe2 />}
            label={t("客户端")}
            value={entry.client_ip || "--"}
          />
          <Metric
            icon={<Server />}
            label={t("站点")}
            value={siteName || entry.site_id}
          />
          <Metric
            icon={<Server />}
            label={t("节点")}
            value={nodeName || entry.node_id}
          />
          <Metric
            icon={<Clock3 />}
            label={t("总耗时")}
            value={`${formatNumber(entry.duration_ms)} ms`}
          />
          <Metric
            icon={<ArrowUpFromLine />}
            label={t("请求大小")}
            value={formatBytes(entry.request_bytes)}
          />
          <Metric
            icon={<ArrowDownToLine />}
            label={t("响应大小")}
            value={formatBytes(entry.bytes)}
          />
          <Metric
            icon={<Globe2 />}
            label={t("协议")}
            value={
              [entry.scheme?.toUpperCase(), entry.protocol]
                .filter(Boolean)
                .join(" · ") || "--"
            }
          />
          <Metric
            icon={<Server />}
            label={t("上游")}
            value={entry.upstream || "--"}
          />
        </CardContent>
      </Card>

      <div className="grid gap-5 xl:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>{t("请求追踪")}</CardTitle>
            <CardDescription>{t("客户端、边缘与源站标识")}</CardDescription>
          </CardHeader>
          <CardContent className="divide-y border-t p-0">
            <DetailRow
              label={t("边缘请求 ID")}
              value={
                <TraceID value={entry.id} copyLabel={t("复制边缘请求 ID")} />
              }
            />
            <DetailRow
              label={t("客户端请求 ID")}
              value={
                <TraceID
                  value={entry.client_request_id}
                  copyLabel={t("复制客户端请求 ID")}
                />
              }
            />
            <DetailRow
              label={t("源站请求 ID")}
              value={
                <TraceID
                  value={entry.upstream_request_id}
                  copyLabel={t("复制源站请求 ID")}
                />
              }
            />
            <DetailRow
              label={t("边缘传输状态")}
              value={
                <StatusBadge
                  status={completion.status}
                  label={completion.label}
                />
              }
            />
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>{t("上游响应")}</CardTitle>
            <CardDescription>{t("源站连接与响应结果")}</CardDescription>
          </CardHeader>
          <CardContent className="divide-y border-t p-0">
            <DetailRow label={t("上游地址")} value={entry.upstream || "--"} />
            <DetailRow
              label={t("上游状态")}
              value={entry.upstream_status || "--"}
            />
            <DetailRow
              label={t("回源建连时间")}
              value={formatUpstreamTime(entry.upstream_connect_time)}
            />
            <DetailRow
              label={t("回源首字节时间")}
              value={formatUpstreamTime(entry.upstream_header_time)}
            />
            <DetailRow
              label={t("回源完整响应时间")}
              value={formatUpstreamTime(entry.upstream_response_time)}
            />
            <DetailRow
              label={t("回源发送大小")}
              value={formatUpstreamBytes(entry.upstream_bytes_sent)}
            />
            <DetailRow
              label={t("回源接收大小")}
              value={formatUpstreamBytes(entry.upstream_bytes_received)}
            />
            <DetailRow
              label={t("响应 Content-Type")}
              value={entry.response_content_type || "--"}
            />
            <DetailRow
              label="Content-Encoding"
              value={entry.content_encoding || "identity"}
            />
            <DetailRow
              label={t("压缩比")}
              value={
                entry.compression_ratio
                  ? `${formatNumber(entry.compression_ratio)}x`
                  : "--"
              }
            />
            <DetailRow
              label={t("压缩节省")}
              value={formatBytes(entry.compression_saved_bytes ?? 0)}
            />
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>{t("请求头")}</CardTitle>
          <CardDescription>{t("边缘节点采集的常用请求头")}</CardDescription>
        </CardHeader>
        <CardContent className="divide-y border-t p-0">
          {headers.length ? (
            headers.map(([name, value]) => (
              <DetailRow key={name} label={name} value={value} />
            ))
          ) : (
            <div className="p-5 text-sm text-muted-foreground">
              {t("暂无请求头数据")}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
function Metric({
  icon,
  label,
  value,
}: {
  icon: ReactNode;
  label: string;
  value: string;
}) {
  return (
    <div className="min-w-0 bg-card p-4">
      <div className="flex items-center gap-2 text-xs text-muted-foreground">
        {icon}
        {label}
      </div>
      <div className="mt-2 truncate text-sm font-medium" title={value}>
        {value}
      </div>
    </div>
  );
}
function DetailRow({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className="grid gap-1 px-5 py-3 sm:grid-cols-[10rem_minmax(0,1fr)] sm:gap-4">
      <span className="text-xs text-muted-foreground">{label}</span>
      <span className="break-all font-mono text-xs leading-5">{value}</span>
    </div>
  );
}
function TraceID({ value, copyLabel }: { value: string; copyLabel: string }) {
  if (!value) return <span>--</span>;
  return (
    <span className="flex min-w-0 items-center gap-2">
      <span className="min-w-0 break-all">{value}</span>
      <CopyButton value={value} label={copyLabel} />
    </span>
  );
}
function completionState(value: string) {
  switch (value?.toUpperCase()) {
    case "OK":
      return { status: "completed", label: t("边缘传输已完成") };
    case "INTERRUPTED":
      return { status: "failed", label: t("边缘传输中断") };
    default:
      return { status: "pending", label: t("未知") };
  }
}
function formatUpstreamTime(value: string) {
  if (!value || value === "-") return "--";
  return value
    .split(/([,:])/)
    .map((part) => {
      const seconds = Number(part.trim());
      return Number.isFinite(seconds)
        ? `${formatNumber(seconds * 1000)} ms`
        : part;
    })
    .join("");
}
function formatUpstreamBytes(value: string) {
  if (!value || value === "-") return "--";
  return value
    .split(/([,:])/)
    .map((part) => {
      const bytes = Number(part.trim());
      return Number.isFinite(bytes) ? formatBytes(bytes) : part;
    })
    .join("");
}
function nameFor(
  items:
    | Array<{
        id: string;
        name: string;
      }>
    | undefined,
  id: string,
) {
  return items?.find((item) => item.id === id)?.name ?? id;
}
