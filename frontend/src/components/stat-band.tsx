import type { LucideIcon } from "lucide-react";
import type { ReactNode } from "react";
import { cn } from "@/lib/utils";
import { toneText, type Tone } from "@/lib/tones";

/**
 * Unified headline-stat band: one bordered surface split into hairline
 * divided cells, so every workspace renders its KPIs the same way.
 * Color is reserved for meaning — pass a tone only when the stat itself
 * carries status (danger/warning); decorative icon coloring stays neutral.
 */
export function StatBand({
  children,
  className,
}: {
  children: ReactNode;
  className?: string;
}) {
  return (
    <div
      data-slot="stat-band"
      className={cn(
        "grid content-stretch gap-px overflow-hidden border-y bg-border",
        className,
      )}
    >
      {children}
    </div>
  );
}

export function StatItem({
  icon: Icon,
  label,
  value,
  detail,
  tone = "neutral",
  density = "default",
  className,
}: {
  icon?: LucideIcon;
  label: ReactNode;
  value: ReactNode;
  detail?: ReactNode;
  tone?: Tone;
  density?: "default" | "compact";
  className?: string;
}) {
  return (
    <div
      data-slot="stat-item"
      className={cn(
        "flex min-w-0 flex-col justify-center gap-2.5 bg-background px-4 sm:px-5",
        density === "default" ? "py-4 sm:py-5" : "px-4 py-3 sm:px-4",
        className,
      )}
    >
      <div className="flex min-w-0 items-center gap-2">
        {Icon ? (
          <span className="grid size-4 shrink-0 place-items-center">
            <Icon className={cn("size-4", toneText[tone])} aria-hidden="true" />
          </span>
        ) : null}
        <p className="min-w-0 break-words text-xs text-muted-foreground">
          {label}
        </p>
      </div>
      <div className="min-w-0">
        <p
          className={cn(
            "break-words leading-snug tabular-nums",
            density === "default"
              ? "text-2xl font-semibold"
              : "text-sm font-medium",
          )}
        >
          {value}
        </p>
        {detail ? (
          <p className="mt-1.5 break-words text-xs text-muted-foreground">
            {detail}
          </p>
        ) : null}
      </div>
    </div>
  );
}
