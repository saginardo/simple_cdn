import { useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import type { NodeDetail, NodeMachineStatus } from "@/lib/types";

export function useMachineStatusStream(nodeId: string, enabled: boolean) {
  const queryClient = useQueryClient();

  useEffect(() => {
    if (!nodeId || !enabled) return;

    const source = new EventSource(
      `/api/nodes/${encodeURIComponent(nodeId)}/machine-status/events`,
    );
    const handleStatus = (event: MessageEvent<string>) => {
      try {
        const machine = JSON.parse(event.data) as NodeMachineStatus;
        if (typeof machine.available !== "boolean") return;
        queryClient.setQueryData<NodeDetail>(["node", nodeId], (current) => {
          if (!current) return current;
          const currentCollectedAt = machineStatusTime(current.machine);
          const incomingCollectedAt = machineStatusTime(machine);
          if (
            currentCollectedAt !== undefined &&
            incomingCollectedAt !== undefined &&
            currentCollectedAt > incomingCollectedAt
          ) {
            return current;
          }
          return { ...current, machine };
        });
      } catch {
        // Ignore malformed events and let the fallback query restore state.
      }
    };

    source.addEventListener("machine-status", handleStatus);
    return () => {
      source.removeEventListener("machine-status", handleStatus);
      source.close();
    };
  }, [enabled, nodeId, queryClient]);
}

export function machineStatusTime(status: NodeMachineStatus) {
  if (!status.report) return undefined;
  const timestamp = Date.parse(status.report.collected_at);
  return Number.isFinite(timestamp) ? timestamp : undefined;
}
