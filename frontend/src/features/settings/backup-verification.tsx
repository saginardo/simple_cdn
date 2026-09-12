import { useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { StatusBadge } from "@/components/status-badge";
import { api, errorMessage } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import type { BackupHealthStatus, RestoreJob } from "@/lib/types";

export function BackupVerification({
  health,
  restoreActive,
}: {
  health: BackupHealthStatus;
  restoreActive: boolean;
}) {
  const client = useQueryClient();
  const verification = health.verification;
  const active = ["queued", "downloading", "validating"].includes(
    verification.state,
  );
  const refresh = () => {
    void client.invalidateQueries({ queryKey: ["backup-health"] });
    void client.invalidateQueries({ queryKey: ["restore-job"] });
  };
  const verify = useMutation({
    mutationFn: () =>
      api<RestoreJob>("/api/backups/verify", { method: "POST" }),
    onSuccess: () => {
      refresh();
      toast.success(t("恢复校验已开始"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const cancel = useMutation({
    mutationFn: () =>
      api<RestoreJob>(
        `/api/backups/restores/${encodeURIComponent(verification.job_id ?? "")}`,
        { method: "DELETE" },
      ),
    onSuccess: refresh,
    onError: (error) => toast.error(errorMessage(error)),
  });
  const state = active
    ? "applying"
    : verification.state === "verified"
      ? "succeeded"
      : verification.state === "failed"
        ? "failed"
        : "pending";
  const label = active
    ? t("校验中")
    : verification.state === "verified"
      ? t("校验通过")
      : verification.state === "failed"
        ? t("校验失败")
        : verification.state === "cancelled"
          ? t("已取消")
          : t("尚未校验");
  return (
    <div className="space-y-3 rounded-lg border p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <span className="text-sm font-medium">{t("恢复校验")}</span>
          <StatusBadge status={state} label={label} />
        </div>
        {active ? (
          <Button
            size="sm"
            variant="outline"
            disabled={cancel.isPending}
            onClick={() => cancel.mutate()}
          >
            {t("取消校验")}
          </Button>
        ) : (
          <Button
            size="sm"
            variant="outline"
            disabled={
              restoreActive ||
              verify.isPending ||
              health.state === "disabled" ||
              verification.state === "unavailable"
            }
            onClick={() => verify.mutate()}
          >
            {t("立即校验最新快照")}
          </Button>
        )}
      </div>
      <p className="text-xs text-muted-foreground">
        {t("定期下载备份并执行隔离恢复校验，完成后清理临时数据。")}
      </p>
      {health.last_succeeded_at ? (
        <p className="text-sm">
          {t("最近成功备份：")}
          {formatDateTime(health.last_succeeded_at)}
        </p>
      ) : null}
      {verification.last_verified_at ? (
        <div className="space-y-1 text-sm">
          <p>
            {t("最近校验通过：")}
            {formatDateTime(verification.last_verified_at)}
          </p>
          <p>
            {t("可恢复至：")}
            {formatDateTime(verification.last_verified_snapshot_time)}{" "}
            <code className="text-xs">
              {verification.last_verified_snapshot_id?.slice(0, 8)}
            </code>
          </p>
        </div>
      ) : null}
      {health.verification_overdue && !active ? (
        <p className="text-sm text-amber-700 dark:text-amber-400">
          {t("恢复校验已到期，等待自动执行或手动启动。")}
        </p>
      ) : null}
      {verification.error ? (
        <p className="break-words text-sm text-destructive">
          {verification.error}
        </p>
      ) : null}
    </div>
  );
}
