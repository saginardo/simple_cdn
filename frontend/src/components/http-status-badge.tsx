import { Badge } from "@/components/ui/badge";
import { httpStatusTone, toneBadge } from "@/lib/tones";
import { cn } from "@/lib/utils";

export function HTTPStatusBadge({ status }: { status: number }) {
  return (
    <Badge
      variant="outline"
      className={cn(
        "font-normal tabular-nums",
        toneBadge[httpStatusTone(status)],
      )}
    >
      {status}
    </Badge>
  );
}
