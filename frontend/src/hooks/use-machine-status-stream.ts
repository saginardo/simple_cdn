import { useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import type { NodeDetail, NodeMachineStatus } from "@/lib/types";

export function useMachineStatusStream(nodeId: string, enabled: boolean) {
  const queryClient = useQueryClient();

  useEffect(() => {
    if (!nodeId || !enabled) return;

    let source: EventSource | undefined;
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

    const disconnect = () => {
      if (!source) return;
      source.removeEventListener("machine-status", handleStatus);
      source.close();
      source = undefined;
    };
    const syncVisibility = () => {
      if (document.visibilityState !== "visible") {
        disconnect();
        return;
      }
      if (source) return;
      source = new EventSource(
        `/api/nodes/${encodeURIComponent(nodeId)}/machine-status/events`,
      );
      source.addEventListener("machine-status", handleStatus);
    };

    document.addEventListener("visibilitychange", syncVisibility);
    syncVisibility();
    return () => {
      document.removeEventListener("visibilitychange", syncVisibility);
      disconnect();
    };
  }, [enabled, nodeId, queryClient]);
}

export function machineStatusTime(status: NodeMachineStatus) {
  const timestamps = [
    status.report?.collected_at,
    status.network?.collected_at,
    status.origin_collected_at,
  ]
    .map((value) => Date.parse(value ?? ""))
    .filter(Number.isFinite);
  return timestamps.length ? Math.max(...timestamps) : undefined;
}
