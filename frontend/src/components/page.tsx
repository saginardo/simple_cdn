import type { ReactNode } from "react";
import { AlertCircle, Inbox } from "lucide-react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";
import { t } from "@/lib/i18n";
export function PageHeader({
  title,
  description,
  actions,
}: {
  title: string;
  description?: string;
  actions?: ReactNode;
}) {
  return (
    <header className="flex min-w-0 flex-col gap-3 border-b bg-background px-4 py-5 sm:flex-row sm:items-center sm:justify-between sm:px-6 lg:px-8">
      <div className="min-w-0 border-l-[3px] border-primary pl-3">
        <h1 className="text-xl font-semibold leading-tight tracking-normal">
          {title}
        </h1>
        {description ? (
          <p className="mt-1 text-sm text-muted-foreground">{description}</p>
        ) : null}
      </div>
      {actions ? (
        <div className="flex shrink-0 self-end flex-wrap items-center gap-2 sm:self-auto">
          {actions}
        </div>
      ) : null}
    </header>
  );
}
export function PageBody({
  className,
  children,
}: {
  className?: string;
  children: ReactNode;
}) {
  return (
    <div
      className={cn(
        "min-w-0 flex-1 space-y-4 p-4 sm:p-6 lg:p-8 animate-in fade-in-50 slide-in-from-bottom-1 duration-200",
        className,
      )}
    >
      {children}
    </div>
  );
}

/**
 * Framed surface for tables and stat grids that sit directly on the page body.
 * Mirrors Card's radius and ring so both container styles read as one system.
 */
export function Panel({
  className,
  children,
}: {
  className?: string;
  children: ReactNode;
}) {
  return (
    <div
      className={cn(
        "overflow-hidden rounded-lg border bg-card text-sm text-card-foreground shadow-xs dark:shadow-[0_1px_0_0_rgba(255,255,255,0.05)_inset]",
        className,
      )}
    >
      {children}
    </div>
  );
}
export function PageLoading({ rows = 4 }: { rows?: number }) {
  return (
    <div className="space-y-3" role="status" aria-live="polite" aria-busy>
      <span className="sr-only">{t("正在加载")}</span>
      {Array.from({
        length: rows,
      }).map((_, index) => (
        <Skeleton
          key={index}
          className={cn("h-16 w-full", index === 0 && "h-28")}
          aria-hidden="true"
        />
      ))}
    </div>
  );
}
export function PageError({
  title = t("加载失败"),
  error,
}: {
  title?: string;
  error: unknown;
}) {
  return (
    <Alert variant="destructive">
      <AlertCircle />
      <AlertTitle>{title}</AlertTitle>
      <AlertDescription>
        {error instanceof Error ? error.message : t("发生未知错误")}
      </AlertDescription>
    </Alert>
  );
}
export function EmptyState({
  title,
  description,
  icon: Icon = Inbox,
  action,
}: {
  title: string;
  description?: string;
  icon?: React.ElementType;
  action?: ReactNode;
}) {
  return (
    <div className="flex min-h-48 flex-col items-center justify-center rounded-xl border border-dashed bg-muted/15 px-6 py-10 text-center transition-colors">
      <div className="mb-3.5 flex size-12 items-center justify-center rounded-full bg-background border shadow-2xs text-muted-foreground/80">
        <Icon className="size-6 stroke-[1.5]" aria-hidden="true" />
      </div>
      <h3 className="text-sm font-semibold text-foreground">{title}</h3>
      {description ? (
        <p className="mt-1.5 max-w-sm text-xs text-muted-foreground leading-relaxed">
          {description}
        </p>
      ) : null}
      {action ? <div className="mt-4">{action}</div> : null}
    </div>
  );
}
