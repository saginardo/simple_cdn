import { useQuery } from "@tanstack/react-query";
import { ChevronLeft, ChevronRight, RotateCcw, Search } from "lucide-react";
import { useId, useMemo, useState, type FormEvent } from "react";
import { useNavigate } from "react-router";
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
import { Card, CardContent } from "@/components/ui/card";
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
import { api } from "@/lib/api";
import { formatBytes, formatDateTime, formatNumber } from "@/lib/format";
import type { LogPage, Node, Site } from "@/lib/types";
import { t, useI18n } from "@/lib/i18n";
interface LogFilters {
  range: string;
  site_id: string;
  node_id: string;
  method: string;
  status: string;
  request_id: string;
  path: string;
  client_ip: string;
  cache_status: string;
}
const defaults: LogFilters = {
  range: "1",
  site_id: "",
  node_id: "",
  method: "",
  status: "",
  request_id: "",
  path: "",
  client_ip: "",
  cache_status: "",
};
export function LogsPage() {
  useI18n();
  const navigate = useNavigate();
  const [draft, setDraft] = useState<LogFilters>(defaults);
  const [search, setSearch] = useState(() => appliedSearch(defaults));
  const [offset, setOffset] = useState(0);
  const sites = useQuery({
    queryKey: ["sites"],
    queryFn: () => api<Site[]>("/api/sites"),
  });
  const nodes = useQuery({
    queryKey: ["nodes"],
    queryFn: () => api<Node[]>("/api/nodes"),
  });
  const url = useMemo(() => logSearchURL(search, offset), [search, offset]);
  const logs = useQuery({
    queryKey: ["logs", url],
    queryFn: () => api<LogPage>(url),
  });
  function submit(event: FormEvent) {
    event.preventDefault();
    setOffset(0);
    setSearch(appliedSearch(draft));
  }
  function reset() {
    setDraft(defaults);
    setOffset(0);
    setSearch(appliedSearch(defaults));
  }
  return (
    <>
      <PageHeader
        title={t("日志")}
        description={t("检索最近 7 天的边缘访问日志")}
        actions={
          <span className="text-xs text-muted-foreground">
            {t("每页 20 条")}
          </span>
        }
      />
      <PageBody>
        <Card>
          <CardContent className="p-4 sm:p-5">
            <form className="grid gap-4" onSubmit={submit}>
              <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4 xl:grid-cols-6">
                <FilterSelect
                  label={t("时间范围")}
                  value={draft.range}
                  onChange={(range) =>
                    setDraft({
                      ...draft,
                      range,
                    })
                  }
                  options={[
                    ["1", t("最近 1 小时")],
                    ["6", t("最近 6 小时")],
                    ["24", t("最近 24 小时")],
                    ["168", t("最近 7 天")],
                  ]}
                />
                <FilterSelect
                  label={t("站点")}
                  value={draft.site_id || "all"}
                  onChange={(value) =>
                    setDraft({
                      ...draft,
                      site_id: value === "all" ? "" : value,
                    })
                  }
                  options={[
                    ["all", t("全部站点")],
                    ...(sites.data ?? []).map((site) => [site.id, site.name]),
                  ]}
                />
                <FilterSelect
                  label={t("节点")}
                  value={draft.node_id || "all"}
                  onChange={(value) =>
                    setDraft({
                      ...draft,
                      node_id: value === "all" ? "" : value,
                    })
                  }
                  options={[
                    ["all", t("全部节点")],
                    ...(nodes.data ?? []).map((node) => [node.id, node.name]),
                  ]}
                />
                <FilterSelect
                  label={t("方法")}
                  value={draft.method || "all"}
                  onChange={(value) =>
                    setDraft({
                      ...draft,
                      method: value === "all" ? "" : value,
                    })
                  }
                  options={[
                    ["all", t("全部方法")],
                    ["GET", "GET"],
                    ["POST", "POST"],
                    ["PUT", "PUT"],
                    ["DELETE", "DELETE"],
                    ["HEAD", "HEAD"],
                  ]}
                />
                <FilterSelect
                  label={t("状态")}
                  value={draft.status || "all"}
                  onChange={(value) =>
                    setDraft({
                      ...draft,
                      status: value === "all" ? "" : value,
                    })
                  }
                  options={[
                    ["all", t("全部状态")],
                    ["2xx", "2xx"],
                    ["3xx", "3xx"],
                    ["4xx", "4xx"],
                    ["5xx", "5xx"],
                  ]}
                />
                <FilterSelect
                  label={t("缓存")}
                  value={draft.cache_status || "all"}
                  onChange={(value) =>
                    setDraft({
                      ...draft,
                      cache_status: value === "all" ? "" : value,
                    })
                  }
                  options={[
                    ["all", t("全部状态")],
                    ["HIT", "HIT"],
                    ["MISS", "MISS"],
                    ["BYPASS", "BYPASS"],
                    ["EXPIRED", "EXPIRED"],
                    ["STALE", "STALE"],
                    ["UPDATING", "UPDATING"],
                    ["REVALIDATED", "REVALIDATED"],
                  ]}
                />
              </div>
              <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-[minmax(12rem,1fr)_minmax(0,2fr)_minmax(12rem,1fr)_auto]">
                <div className="grid gap-1.5">
                  <Label htmlFor="log-request-id">{t("请求 ID")}</Label>
                  <Input
                    id="log-request-id"
                    maxLength={256}
                    placeholder="req_01..."
                    value={draft.request_id}
                    onChange={(event) =>
                      setDraft({
                        ...draft,
                        request_id: event.target.value,
                      })
                    }
                  />
                </div>
                <div className="grid gap-1.5">
                  <Label htmlFor="log-path">{t("路径包含")}</Label>
                  <Input
                    id="log-path"
                    maxLength={512}
                    placeholder="/api/"
                    value={draft.path}
                    onChange={(event) =>
                      setDraft({
                        ...draft,
                        path: event.target.value,
                      })
                    }
                  />
                </div>
                <div className="grid gap-1.5">
                  <Label htmlFor="log-ip">{t("客户端 IP")}</Label>
                  <Input
                    id="log-ip"
                    placeholder="203.0.113.10"
                    value={draft.client_ip}
                    onChange={(event) =>
                      setDraft({
                        ...draft,
                        client_ip: event.target.value,
                      })
                    }
                  />
                </div>
                <div className="flex items-end gap-2">
                  <Button type="submit">
                    <Search />
                    {t("搜索")}
                  </Button>
                  <Button
                    type="button"
                    variant="outline"
                    size="icon"
                    aria-label={t("重置筛选")}
                    onClick={reset}
                  >
                    <RotateCcw />
                  </Button>
                </div>
              </div>
            </form>
          </CardContent>
        </Card>

        {logs.isLoading ? <PageLoading rows={3} /> : null}
        {logs.error ? (
          <PageError title={t("日志检索失败")} error={logs.error} />
        ) : null}
        {logs.data ? (
          <Card>
            <CardContent className="p-0">
              {logs.data.logs.length ? (
                <Table className="table-fixed min-w-[1120px]">
                  <TableHeader>
                    <TableRow>
                      <TableHead className="w-40 pl-5">{t("时间")}</TableHead>
                      <TableHead className="w-[26rem]">{t("请求")}</TableHead>
                      <TableHead className="w-20">{t("状态")}</TableHead>
                      <TableHead className="w-40">{t("客户端")}</TableHead>
                      <TableHead className="w-48">{t("站点 / 节点")}</TableHead>
                      <TableHead className="w-24">{t("缓存")}</TableHead>
                      <TableHead className="w-32 pr-5 text-right">
                        {t("耗时 / 大小")}
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {logs.data.logs.map((entry, index) => (
                      <TableRow
                        key={
                          entry.id ||
                          `${entry.timestamp}-${entry.node_id}-${index}`
                        }
                        className={
                          entry.id
                            ? "cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset"
                            : undefined
                        }
                        tabIndex={entry.id ? 0 : undefined}
                        aria-label={
                          entry.id
                            ? t("查看请求 {value0} {value1}", {
                                value0: entry.method,
                                value1: entry.path,
                              })
                            : undefined
                        }
                        onClick={() => {
                          if (entry.id)
                            navigate(`/logs/${encodeURIComponent(entry.id)}`);
                        }}
                        onKeyDown={(event) => {
                          if (
                            entry.id &&
                            (event.key === "Enter" || event.key === " ")
                          ) {
                            event.preventDefault();
                            navigate(`/logs/${encodeURIComponent(entry.id)}`);
                          }
                        }}
                      >
                        <TableCell className="whitespace-nowrap pl-5 text-xs text-muted-foreground">
                          {formatDateTime(entry.timestamp)}
                        </TableCell>
                        <TableCell className="min-w-0 overflow-hidden">
                          <div className="flex min-w-0 items-center gap-2 overflow-hidden">
                            <span className="shrink-0 font-mono text-xs font-medium">
                              {entry.method}
                            </span>
                            <code
                              className="block min-w-0 truncate text-xs"
                              title={entry.path}
                            >
                              {entry.path}
                            </code>
                          </div>
                        </TableCell>
                        <TableCell>
                          <HTTPStatusBadge status={entry.status} />
                        </TableCell>
                        <TableCell className="font-mono text-xs">
                          {entry.client_ip}
                        </TableCell>
                        <TableCell className="text-xs">
                          <div>{siteName(sites.data, entry.site_id)}</div>
                          <div className="text-muted-foreground">
                            {nodeName(nodes.data, entry.node_id)}
                          </div>
                        </TableCell>
                        <TableCell>
                          <StatusBadge
                            status={entry.cache_status || "UNCACHED"}
                          />
                        </TableCell>
                        <TableCell className="pr-5 text-right text-xs tabular-nums">
                          <div>{formatNumber(entry.duration_ms)} ms</div>
                          <div className="text-muted-foreground">
                            {formatBytes(entry.bytes)}
                          </div>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              ) : (
                <div className="p-6">
                  <EmptyState
                    title={t("没有匹配的日志")}
                    description={t("调整筛选条件后重新搜索")}
                  />
                </div>
              )}
              <div className="flex items-center justify-between border-t px-5 py-3">
                <span className="text-xs text-muted-foreground">
                  {t("第 ")}
                  {Math.floor(logs.data.offset / logs.data.page_size) + 1}
                  {t(" 页 · 当前 ")}
                  {logs.data.logs.length}
                  {t(" 条")}
                </span>
                <div className="flex gap-2">
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={!offset || logs.isFetching}
                    onClick={() =>
                      setOffset(Math.max(0, offset - logs.data.page_size))
                    }
                  >
                    <ChevronLeft />
                    {t("上一页")}
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    disabled={!logs.data.has_more || logs.isFetching}
                    onClick={() => setOffset(offset + logs.data.page_size)}
                  >
                    {t("下一页")}
                    <ChevronRight />
                  </Button>
                </div>
              </div>
            </CardContent>
          </Card>
        ) : null}
      </PageBody>
    </>
  );
}
function FilterSelect({
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
    <div className="grid min-w-0 gap-1.5">
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
function appliedSearch(filters: LogFilters) {
  const to = new Date();
  const from = new Date(to.getTime() - Number(filters.range) * 60 * 60 * 1000);
  return {
    ...filters,
    from: from.toISOString(),
    to: to.toISOString(),
  };
}
function logSearchURL(
  search: ReturnType<typeof appliedSearch>,
  offset: number,
) {
  const params = new URLSearchParams({
    from: search.from,
    to: search.to,
    offset: String(offset),
  });
  for (const key of [
    "site_id",
    "node_id",
    "method",
    "status",
    "request_id",
    "path",
    "client_ip",
    "cache_status",
  ] as const)
    if (search[key]) params.set(key, search[key]);
  return `/api/logs?${params.toString()}`;
}
function siteName(sites: Site[] | undefined, id: string) {
  return sites?.find((site) => site.id === id)?.name ?? id;
}
function nodeName(nodes: Node[] | undefined, id: string) {
  return nodes?.find((node) => node.id === id)?.name ?? id;
}
