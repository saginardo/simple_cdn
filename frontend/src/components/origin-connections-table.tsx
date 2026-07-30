import { StatusBadge } from "@/components/status-badge";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { formatDateTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import type { OriginProbeSample, OriginProbeStatus } from "@/lib/types";

export interface OriginConnectionContext {
  key: string;
  label: string;
  detail?: string;
}

export function OriginConnectionsTable({
  probes,
  contextHeading,
  contexts,
}: {
  probes: OriginProbeStatus[];
  contextHeading: string;
  contexts: (probe: OriginProbeStatus) => OriginConnectionContext[];
}) {
  return (
    <div className="w-full min-w-0 max-w-full overflow-x-auto rounded-md border">
      <Table className="min-w-[980px]">
        <TableHeader>
          <TableRow>
            <TableHead className="w-28">{t("状态")}</TableHead>
            <TableHead>{t("源站")}</TableHead>
            <TableHead>{contextHeading}</TableHead>
            <TableHead className="text-right">{t("池容量")}</TableHead>
            <TableHead>{t("服务探测")}</TableHead>
            <TableHead>{t("冷连接")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {probes.map((probe) => {
            const state = originProbeState(probe);
            const context = contexts(probe);
            return (
              <TableRow key={probe.pool_id}>
                <TableCell>
                  <StatusBadge status={state.status} label={state.label} />
                </TableCell>
                <TableCell className="max-w-64">
                  <div className="font-mono text-xs font-medium">
                    {probe.address}
                  </div>
                  <div className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
                    <span className="uppercase">{probe.scheme}</span>
                    {probe.scheme === "http" || probe.scheme === "https" ? (
                      <span>
                        ·{" "}
                        {probe.http_version === "h2c"
                          ? "H2C"
                          : probe.http_version === "http2"
                            ? "HTTP/2"
                            : "HTTP/1.1"}
                      </span>
                    ) : null}
                  </div>
                </TableCell>
                <TableCell className="max-w-56 text-xs">
                  {context.length
                    ? context.map((item) => (
                        <div key={item.key}>
                          {item.label}
                          {item.detail ? (
                            <span className="ml-1 text-muted-foreground">
                              · {item.detail}
                            </span>
                          ) : null}
                        </div>
                      ))
                    : "--"}
                </TableCell>
                <TableCell className="text-right text-xs tabular-nums">
                  {t("每工作进程 {value0}", {
                    value0: probe.keepalive_connections,
                  })}
                </TableCell>
                <ProbeSampleCell
                  sample={probe.service_probe}
                  scheme={probe.scheme}
                />
                <ProbeSampleCell
                  sample={probe.cold_probe}
                  scheme={probe.scheme}
                />
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </div>
  );
}

export function normalOriginProbeCount(probes: OriginProbeStatus[]) {
  return probes.filter(
    (probe) => probe.healthy && probe.circuit_state === "closed",
  ).length;
}

export function latestOriginProbeTime(probes: OriginProbeStatus[]) {
  return probes.reduce<string | undefined>((latest, probe) => {
    if (!latest || Date.parse(probe.checked_at) > Date.parse(latest)) {
      return probe.checked_at;
    }
    return latest;
  }, undefined);
}

function originProbeState(probe: OriginProbeStatus) {
  if (probe.circuit_state === "open")
    return { status: "failed", label: t("已熔断") };
  if (probe.circuit_state === "recovering")
    return { status: "pending", label: t("恢复确认") };
  if (!probe.healthy) return { status: "failed", label: t("探测异常") };
  return { status: "active", label: t("正常") };
}

function ProbeSampleCell({
  sample,
  scheme,
}: {
  sample?: OriginProbeSample;
  scheme: OriginProbeStatus["scheme"];
}) {
  if (!sample) {
    return (
      <TableCell className="min-w-56 text-xs text-muted-foreground">
        --
      </TableCell>
    );
  }
  const timings = [
    sample.connect_ms > 0
      ? `TCP ${formatProbeTiming(sample.connect_ms)}`
      : null,
    sample.tls_handshake_ms > 0
      ? `TLS ${formatProbeTiming(sample.tls_handshake_ms)}`
      : null,
    sample.header_ms > 0
      ? `${scheme.startsWith("grpc") ? "RPC" : "TTFB"} ${formatProbeTiming(sample.header_ms)}`
      : null,
    `${t("总耗时")} ${formatProbeTiming(sample.total_ms)}`,
  ].filter(Boolean);
  return (
    <TableCell className="min-w-56 align-top text-xs">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <span
          aria-hidden="true"
          className={`size-1.5 rounded-full ${sample.healthy ? "bg-success" : "bg-destructive"}`}
        />
        <span className="font-medium">
          {sample.healthy ? t("可用") : t("异常")}
        </span>
        <span className="text-muted-foreground">
          {sample.connection_reused ? t("复用连接") : t("新建连接")}
        </span>
        {sample.http_status ? (
          <span className="font-mono tabular-nums text-muted-foreground">
            HTTP {sample.http_status}
          </span>
        ) : null}
      </div>
      <div className="mt-1 font-mono text-[11px] leading-5 tabular-nums text-muted-foreground">
        {timings.join(" · ")}
      </div>
      {sample.error ? (
        <p
          className="mt-1 max-w-64 truncate text-xs text-destructive"
          title={sample.error}
        >
          {sample.error}
        </p>
      ) : null}
      <div className="mt-1 text-[11px] text-muted-foreground">
        {formatDateTime(sample.checked_at)}
      </div>
    </TableCell>
  );
}

function formatProbeTiming(value: number) {
  return value > 0
    ? `${value < 1 ? value.toFixed(2) : value.toFixed(1)} ms`
    : "0 ms";
}
