export type Tone = "success" | "warning" | "info" | "danger" | "neutral";

/** Outlined badge surfaces. */
export const toneBadge: Record<Tone, string> = {
  success: "border-success/25 bg-success/10 text-success",
  warning: "border-warning/25 bg-warning/10 text-warning",
  info: "border-info/25 bg-info/10 text-info",
  danger: "border-destructive/25 bg-destructive/10 text-destructive",
  neutral: "border-border bg-muted text-muted-foreground",
};

/** Standalone icons and inline emphasis text. */
export const toneText: Record<Tone, string> = {
  success: "text-success",
  warning: "text-warning",
  info: "text-info",
  danger: "text-destructive",
  neutral: "text-muted-foreground",
};

/** Tinted square chips holding an icon. */
export const toneSurface: Record<Tone, string> = {
  success: "bg-success/10 text-success",
  warning: "bg-warning/10 text-warning",
  info: "bg-info/10 text-info",
  danger: "bg-destructive/10 text-destructive",
  neutral: "bg-muted text-muted-foreground",
};

/** Solid fills for progress bars, dots and other non-text marks. */
export const toneFill: Record<Tone, string> = {
  success: "bg-success",
  warning: "bg-warning",
  info: "bg-info",
  danger: "bg-destructive",
  neutral: "bg-muted-foreground",
};

/** Maps an HTTP status code to its tone. */
export function httpStatusTone(status: number): Tone {
  if (status >= 500) return "danger";
  if (status >= 400) return "warning";
  if (status >= 300) return "info";
  if (status >= 200) return "success";
  return "neutral";
}
