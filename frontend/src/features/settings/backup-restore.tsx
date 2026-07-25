import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  DatabaseBackup,
  LoaderCircle,
  RefreshCw,
  RotateCcw,
  ShieldCheck,
  Trash2,
  X,
} from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";
import { ConfirmDialog } from "@/components/confirm-dialog";
import { ListPagination } from "@/components/list-pagination";
import { EmptyState, PageError, Panel } from "@/components/page";
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
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { api, errorMessage } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import type { BackupRunStatus, RestoreJob, RestoreSnapshot } from "@/lib/types";
import { useListPagination } from "@/hooks/use-list-pagination";
import { t } from "@/lib/i18n";
export function BackupRestore() {
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<RestoreSnapshot | null>(null);
  const [deleting, setDeleting] = useState<RestoreSnapshot | null>(null);
  const [commitOpen, setCommitOpen] = useState(false);
  const [cancelOpen, setCancelOpen] = useState(false);
  const status = useQuery({
    queryKey: ["backup-status"],
    queryFn: () => api<BackupRunStatus | null>("/api/backups/status"),
    refetchInterval: 30_000,
  });
  const snapshots = useQuery({
    queryKey: ["backup-snapshots"],
    queryFn: () => api<RestoreSnapshot[]>("/api/backups/snapshots"),
  });
  const snapshotsPagination = useListPagination(snapshots.data ?? []);
  const job = useQuery({
    queryKey: ["restore-job"],
    queryFn: () => api<RestoreJob | null>("/api/backups/restores/current"),
    refetchInterval: (query) =>
      activeRestore(query.state.data?.state) ? 2_000 : 15_000,
  });
  const refresh = () => {
    void status.refetch();
    void snapshots.refetch();
    void job.refetch();
  };
  const updateJob = (next: RestoreJob) => {
    queryClient.setQueryData(["restore-job"], next);
    void queryClient.invalidateQueries({
      queryKey: ["messages"],
    });
  };
  const start = useMutation({
    mutationFn: (snapshot: RestoreSnapshot) =>
      api<RestoreJob>("/api/backups/restores", {
        method: "POST",
        body: JSON.stringify({
          snapshot_id: snapshot.id,
          confirmation: snapshot.short_id,
        }),
      }),
    onSuccess: (next) => {
      updateJob(next);
      setSelected(null);
      toast.success(t("快照下载与隔离校验已开始"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const commit = useMutation({
    mutationFn: () =>
      api<RestoreJob>(
        `/api/backups/restores/${encodeURIComponent(job.data?.id ?? "")}/commit`,
        {
          method: "POST",
          body: JSON.stringify({
            confirmation: "RESTORE",
          }),
        },
      ),
    onSuccess: (next) => {
      updateJob(next);
      setCommitOpen(false);
      toast.success(t("恢复切换已提交，控制面将短暂重启"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const cancel = useMutation({
    mutationFn: () =>
      api<RestoreJob>(
        `/api/backups/restores/${encodeURIComponent(job.data?.id ?? "")}`,
        {
          method: "DELETE",
        },
      ),
    onSuccess: (next) => {
      updateJob(next);
      setCancelOpen(false);
      toast.success(t("在线恢复已取消"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const deleteSnapshot = useMutation({
    mutationFn: (snapshot: RestoreSnapshot) =>
      api<{
        deleted_snapshot_id: string;
      }>(`/api/backups/snapshots/${encodeURIComponent(snapshot.id)}`, {
        method: "DELETE",
        body: JSON.stringify({
          confirmation: snapshot.short_id,
        }),
      }),
    onSuccess: (_response, snapshot) => {
      queryClient.setQueryData<RestoreSnapshot[]>(
        ["backup-snapshots"],
        (currentSnapshots) =>
          currentSnapshots?.filter((item) => item.id !== snapshot.id),
      );
      void queryClient.invalidateQueries({
        queryKey: ["backup-snapshots"],
      });
      setDeleting(null);
      if (selected?.id === snapshot.id) setSelected(null);
      toast.success(
        t("备份快照 {value0} 已永久删除", {
          value0: snapshot.short_id,
        }),
      );
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const current = job.data;
  const busy =
    start.isPending ||
    commit.isPending ||
    cancel.isPending ||
    deleteSnapshot.isPending;
  return (
    <>
      <Card>
        <CardHeader className="flex-row items-start justify-between gap-4">
          <div>
            <CardTitle>{t("S3 在线恢复")}</CardTitle>
            <CardDescription>
              {t("下载到隔离环境校验，通过后再切换控制面数据")}
            </CardDescription>
          </div>
          <Button
            variant="outline"
            size="icon-sm"
            aria-label={t("刷新备份与快照")}
            disabled={
              status.isFetching || snapshots.isFetching || job.isFetching
            }
            onClick={refresh}
          >
            <RefreshCw
              className={
                status.isFetching || snapshots.isFetching || job.isFetching
                  ? "animate-spin"
                  : undefined
              }
            />
          </Button>
        </CardHeader>
        <CardContent className="space-y-5">
          {status.error ? (
            <PageError title={t("备份状态加载失败")} error={status.error} />
          ) : null}
          {status.data ? (
            <div className="flex flex-col gap-2 rounded-lg border px-4 py-3 sm:flex-row sm:items-center">
              <StatusBadge
                status={backupState(status.data.state)}
                label={backupLabel(status.data.state)}
              />
              <span className="text-sm">
                {t("最近备份：")}
                {formatDateTime(status.data.updated_at)}
              </span>
              <span className="sm:ml-auto text-xs text-muted-foreground">
                {t("尝试 ")}
                {status.data.attempt} / {status.data.max_attempts}
              </span>
              {status.data.error ? (
                <span className="text-xs text-destructive">
                  {status.data.error}
                </span>
              ) : null}
            </div>
          ) : (
            <div className="rounded-lg border px-4 py-3 text-sm text-muted-foreground">
              {t("尚无备份运行状态")}
            </div>
          )}
          {current ? (
            <RestoreJobPanel
              job={current}
              onCommit={() => setCommitOpen(true)}
              onCancel={() => setCancelOpen(true)}
              busy={busy}
            />
          ) : null}
          {snapshots.error ? (
            <PageError title={t("快照加载失败")} error={snapshots.error} />
          ) : null}
          {snapshots.isLoading ? (
            <div className="py-8 text-center text-sm text-muted-foreground">
              <LoaderCircle className="mx-auto mb-2 size-4 animate-spin" />
              {t("正在读取快照")}
            </div>
          ) : snapshots.data?.length ? (
            <Panel>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("备份时间")}</TableHead>
                    <TableHead>{t("快照")}</TableHead>
                    <TableHead>{t("主机")}</TableHead>
                    <TableHead className="text-right">{t("操作")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {snapshotsPagination.items.map((snapshot) => (
                    <TableRow key={snapshot.id}>
                      <TableCell className="whitespace-nowrap">
                        {formatDateTime(snapshot.time)}
                      </TableCell>
                      <TableCell>
                        <code>{snapshot.short_id}</code>
                      </TableCell>
                      <TableCell>{snapshot.hostname || "--"}</TableCell>
                      <TableCell className="text-right">
                        <div className="flex justify-end gap-1">
                          <Button
                            variant="outline"
                            size="sm"
                            disabled={busy || activeRestore(current?.state)}
                            onClick={() => setSelected(snapshot)}
                          >
                            <ShieldCheck />
                            {t("准备恢复")}
                          </Button>
                          <Tooltip>
                            <TooltipTrigger asChild>
                              <Button
                                variant="destructive"
                                size="icon-sm"
                                aria-label={t("删除快照 {value0}", {
                                  value0: snapshot.short_id,
                                })}
                                disabled={busy || activeRestore(current?.state)}
                                onClick={() => setDeleting(snapshot)}
                              >
                                <Trash2 />
                              </Button>
                            </TooltipTrigger>
                            <TooltipContent>{t("删除备份快照")}</TooltipContent>
                          </Tooltip>
                        </div>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
              <ListPagination
                pagination={snapshotsPagination}
                itemLabel={t("个快照")}
              />
            </Panel>
          ) : (
            <EmptyState
              title={t("没有可用快照")}
              description={t("完成一次 S3 备份后可在此准备在线恢复")}
            />
          )}
        </CardContent>
      </Card>
      <ConfirmDialog
        open={Boolean(selected)}
        onOpenChange={(open) => {
          if (!open) setSelected(null);
        }}
        title={t("准备在线恢复")}
        description={t(
          "将下载并在隔离环境校验快照 {value0}，此阶段不会修改在线数据。",
          {
            value0: selected?.short_id ?? "",
          },
        )}
        confirmation={selected?.short_id}
        confirmLabel={t("下载并校验")}
        busy={start.isPending}
        onConfirm={async () => {
          if (selected) await start.mutateAsync(selected);
        }}
      />
      <ConfirmDialog
        open={Boolean(deleting)}
        onOpenChange={(open) => {
          if (!open) setDeleting(null);
        }}
        title={t("删除备份快照")}
        description={t(
          "将从 S3 备份仓库永久删除快照 {value0}，并清理无引用数据。此操作不可撤销。",
          {
            value0: deleting?.short_id ?? "",
          },
        )}
        confirmation={deleting?.short_id}
        confirmLabel={t("永久删除")}
        destructive
        busy={deleteSnapshot.isPending}
        onConfirm={async () => {
          if (deleting) await deleteSnapshot.mutateAsync(deleting);
        }}
      />
      <ConfirmDialog
        open={commitOpen}
        onOpenChange={setCommitOpen}
        title={t("切换恢复快照")}
        description={t("控制面将暂停相关操作、切换已校验的数据并短暂重启。")}
        confirmation="RESTORE"
        confirmLabel={t("确认切换")}
        destructive
        busy={commit.isPending}
        onConfirm={async () => {
          await commit.mutateAsync();
        }}
      />
      <ConfirmDialog
        open={cancelOpen}
        onOpenChange={setCancelOpen}
        title={t("取消在线恢复")}
        description={t("取消准备流程并删除隔离数据，不会修改当前在线数据。")}
        confirmLabel={t("取消恢复")}
        destructive
        busy={cancel.isPending}
        onConfirm={async () => {
          await cancel.mutateAsync();
        }}
      />
    </>
  );
}
function RestoreJobPanel({
  job,
  onCommit,
  onCancel,
  busy,
}: {
  job: RestoreJob;
  onCommit: () => void;
  onCancel: () => void;
  busy: boolean;
}) {
  const active = activeRestore(job.state);
  return (
    <Alert variant={job.state === "failed" ? "destructive" : "default"}>
      <DatabaseBackup />
      <AlertTitle className="flex items-center gap-2">
        {t("恢复任务")}{" "}
        <StatusBadge status={job.state} label={restoreLabel(job.state)} />
      </AlertTitle>
      <AlertDescription className="mt-2 space-y-2">
        <p>{job.error || job.detail || t("等待状态更新")}</p>
        <div className="text-xs">
          {t("快照 ")}
          {job.snapshot_short_id}
          {t(" · 更新于 ")}
          {formatDateTime(job.updated_at)}
        </div>
        <div className="flex flex-wrap gap-2 pt-2">
          {job.state === "ready" ? (
            <Button size="sm" disabled={busy} onClick={onCommit}>
              <RotateCcw />
              {t("切换数据")}
            </Button>
          ) : null}
          {active && job.state !== "committing" ? (
            <Button
              size="sm"
              variant="outline"
              disabled={busy}
              onClick={onCancel}
            >
              <X />
              {t("取消恢复")}
            </Button>
          ) : null}
        </div>
      </AlertDescription>
    </Alert>
  );
}
function activeRestore(state?: string) {
  return Boolean(
    state &&
    ["queued", "downloading", "validating", "ready", "committing"].includes(
      state,
    ),
  );
}
function restoreLabel(state: string) {
  return (
    (
      {
        queued: t("排队中"),
        downloading: t("下载中"),
        validating: t("隔离校验中"),
        ready: t("校验通过"),
        committing: t("正在切换"),
        completed: t("已完成"),
        failed: t("失败"),
        cancelled: t("已取消"),
      } as Record<string, string>
    )[state] ?? state
  );
}
function backupLabel(state: string) {
  return (
    (
      {
        running: t("执行中"),
        retrying: t("重试中"),
        succeeded: t("成功"),
        failed: t("失败"),
        skipped: t("已跳过"),
      } as Record<string, string>
    )[state] ?? state
  );
}
function backupState(state: string) {
  return (
    (
      {
        running: "applying",
        retrying: "queued",
        succeeded: "succeeded",
        failed: "failed",
        skipped: "pending",
      } as Record<string, string>
    )[state] ?? state
  );
}
