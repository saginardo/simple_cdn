import { Badge } from "@/components/ui/badge";
import { t } from "@/lib/i18n";
import { toneBadge, type Tone } from "@/lib/tones";
import { cn } from "@/lib/utils";

const labels: Record<string, string> = {
  pending: "待激活",
  active: "运行中",
  draining: "已暂停",
  revoked: "已撤销",
  uninstalling: "卸载中",
  uninstalled: "已卸载",
  queued: "排队中",
  dispatching: "分发中",
  applying: "应用中",
  succeeded: "成功",
  partial: "部分成功",
  failed: "失败",
  rolled_back: "已回滚",
  ready: "已就绪",
  committing: "正在切换",
  completed: "已完成",
  cancelled: "已取消",
  canceled: "已取消",
  preparing: "准备中",
  running: "执行中",
  forced: "强制完成",
};

const tones: Record<string, Tone> = {
  active: "success",
  succeeded: "success",
  completed: "success",
  ready: "info",
  applying: "info",
  dispatching: "info",
  running: "info",
  pending: "warning",
  queued: "warning",
  failed: "danger",
  revoked: "danger",
};

export function StatusBadge({
  status,
  label,
}: {
  status: string;
  label?: string;
}) {
  return (
    <Badge
      variant="outline"
      className={cn("font-normal", tones[status] && toneBadge[tones[status]])}
    >
      {t(label ?? labels[status] ?? status)}
    </Badge>
  );
}
