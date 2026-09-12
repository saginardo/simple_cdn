import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Panel } from "@/components/page";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { StatusBadge } from "@/components/status-badge";
import { api, errorMessage } from "@/lib/api";
import { t } from "@/lib/i18n";
import type { Node, NodeUpgradeRollout } from "@/lib/types";

export function UpgradeRolloutDialog({
  open,
  onOpenChange,
  nodes,
}: {
  open: boolean;
  onOpenChange: (value: boolean) => void;
  nodes: Node[];
}) {
  const client = useQueryClient();
  const [canary, setCanary] = useState("");
  const [parallel, setParallel] = useState(1);
  const [window, setWindow] = useState(60);
  const eligible = nodes.filter((node) => node.can_upgrade);
  const start = useMutation({
    mutationFn: () =>
      api<{ created: number; rollout?: NodeUpgradeRollout }>(
        "/api/nodes/upgrade-all",
        {
          method: "POST",
          body: JSON.stringify({
            max_parallel: parallel,
            health_window_seconds: window,
            canary_node_id: canary || eligible[0]?.id,
          }),
        },
      ),
    onSuccess: (result) => {
      void client.invalidateQueries({ queryKey: ["nodes"] });
      void client.invalidateQueries({ queryKey: ["upgrade-rollout"] });
      onOpenChange(false);
      toast.success(t("已纳入 {value0} 个节点", { value0: result.created }));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form
          onSubmit={(event) => {
            event.preventDefault();
            start.mutate();
          }}
          className="space-y-4"
        >
          <DialogHeader>
            <DialogTitle>{t("分批升级")}</DialogTitle>
            <DialogDescription>
              {t(
                "先升级金丝雀节点，通过健康观察后按并发上限继续；任一节点失败即暂停后续派发。",
              )}
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Label htmlFor="rollout-canary">{t("金丝雀节点")}</Label>
            <select
              id="rollout-canary"
              className="h-9 w-full rounded-md border bg-background px-3 text-sm"
              value={canary || eligible[0]?.id || ""}
              onChange={(event) => setCanary(event.target.value)}
              required
            >
              {eligible.map((node) => (
                <option key={node.id} value={node.id}>
                  {node.name}
                </option>
              ))}
            </select>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-2">
              <Label htmlFor="rollout-parallel">{t("最大并发节点")}</Label>
              <Input
                id="rollout-parallel"
                type="number"
                min={1}
                max={10}
                required
                value={parallel}
                onChange={(event) => setParallel(Number(event.target.value))}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="rollout-window">{t("健康观察（秒）")}</Label>
              <Input
                id="rollout-window"
                type="number"
                min={30}
                max={600}
                required
                value={window}
                onChange={(event) => setWindow(Number(event.target.value))}
              />
            </div>
          </div>
          <p className="text-xs text-muted-foreground">
            {t("将纳入 {value0} 个可升级节点，批次创建后固定节点与目标版本。", {
              value0: eligible.length,
            })}
          </p>
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
              disabled={start.isPending || !eligible.length}
            >
              {t("开始分批升级")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

const memberLabels: Record<string, string> = {
  pending: "等待派发",
  upgrading: "升级中",
  verifying: "健康观察中",
  succeeded: "已通过",
  failed: "失败",
  skipped: "已跳过",
};
export function UpgradeRolloutPanel({
  rollout,
}: {
  rollout: NodeUpgradeRollout;
}) {
  const client = useQueryClient();
  const active = rollout.state === "running" || rollout.state === "paused";
  const change = useMutation({
    mutationFn: (action: string) =>
      api<NodeUpgradeRollout>(
        `/api/nodes/upgrade-rollouts/${encodeURIComponent(rollout.id)}/${action}`,
        { method: "POST" },
      ),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ["upgrade-rollout"] });
      void client.invalidateQueries({ queryKey: ["nodes"] });
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  const labels: Record<string, string> = {
    running: "进行中",
    paused: "已暂停",
    succeeded: "已完成",
    cancelled: "已取消",
  };
  return (
    <Panel>
      <div className="space-y-4 p-5">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <h2 className="text-sm font-semibold">{t("分批升级")}</h2>
            <StatusBadge
              status={rollout.state === "paused" ? "failed" : rollout.state}
              label={t(labels[rollout.state] || rollout.state)}
            />
            <span className="text-xs text-muted-foreground">
              {
                rollout.members.filter((member) => member.state === "succeeded")
                  .length
              }{" "}
              / {rollout.members.length}
            </span>
          </div>
          {active ? (
            <div className="flex gap-2">
              <Button
                size="sm"
                variant="outline"
                disabled={change.isPending}
                onClick={() =>
                  change.mutate(rollout.state === "paused" ? "resume" : "pause")
                }
              >
                {t(rollout.state === "paused" ? "恢复升级" : "暂停派发")}
              </Button>
              <Button
                size="sm"
                variant="outline"
                disabled={change.isPending}
                onClick={() => change.mutate("cancel")}
              >
                {t("取消后续升级")}
              </Button>
            </div>
          ) : null}
        </div>
        <p className="text-sm break-words">{t(rollout.detail)}</p>
        <p className="text-xs text-muted-foreground">
          {t("并发上限 {value0} · 健康观察 {value1} 秒", {
            value0: rollout.max_parallel,
            value1: rollout.health_window_seconds,
          })}{" "}
          · {t("暂停或取消后，已下发的升级仍会执行。")}
        </p>
        <div className="max-h-64 divide-y overflow-y-auto rounded-md border">
          {rollout.members.map((member, index) => (
            <div
              key={member.node_id}
              className="flex flex-wrap items-center justify-between gap-2 px-3 py-2"
            >
              <div className="min-w-0">
                <div className="text-sm font-medium break-all">
                  {member.name}
                  {index === 0 ? (
                    <span className="ml-2 text-xs text-muted-foreground">
                      {t("金丝雀")}
                    </span>
                  ) : null}
                </div>
                {member.detail ? (
                  <div className="text-xs text-muted-foreground break-words">
                    {t(member.detail)}
                  </div>
                ) : null}
              </div>
              <StatusBadge
                status={
                  member.state === "verifying" ? "applying" : member.state
                }
                label={t(memberLabels[member.state] || member.state)}
              />
            </div>
          ))}
        </div>
      </div>
    </Panel>
  );
}
