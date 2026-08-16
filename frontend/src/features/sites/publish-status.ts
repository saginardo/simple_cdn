import type { DeploymentTask, PublishStatus, Site } from "@/lib/types";

const lateConfirmationPollingWindowMs = 2 * 60 * 1_000;

export function activeTask(task?: DeploymentTask | null) {
  return Boolean(
    task && ["queued", "dispatching", "applying"].includes(task.status),
  );
}

export function taskMatchesCurrentSite(
  task: DeploymentTask | null | undefined,
  site: Site,
) {
  if (!task) return false;
  if (site.deleting || site.published || activeTask(task)) return true;
  const taskCreatedAt = Date.parse(task.created_at);
  const siteUpdatedAt = Date.parse(site.updated_at);
  return (
    Number.isFinite(taskCreatedAt) &&
    Number.isFinite(siteUpdatedAt) &&
    taskCreatedAt >= siteUpdatedAt
  );
}

export function shouldPollPublishStatusFast(
  status?: PublishStatus,
  currentTime = Date.now(),
) {
  if (activeTask(status?.task)) return true;
  const task = status?.task;
  if (!task || !["partial", "failed"].includes(task.status)) return false;
  if (
    !status.nodes.some(
      (node) => node.status === "failed" || node.status === "timed_out",
    )
  )
    return false;
  const completionTime = Date.parse(task.deadline_at ?? task.updated_at);
  return (
    Number.isFinite(completionTime) &&
    currentTime <= completionTime + lateConfirmationPollingWindowMs
  );
}
