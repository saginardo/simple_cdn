import type { DeploymentTask, Site } from "@/lib/types";

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
