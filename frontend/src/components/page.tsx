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
      className={cn("min-w-0 flex-1 space-y-4 p-4 sm:p-6 lg:p-8", className)}
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
        "overflow-hidden rounded-lg border bg-card text-sm text-card-foreground",
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
}: {
  title: string;
  description?: string;
}) {
  return (
    <div className="flex min-h-44 flex-col items-center justify-center rounded-lg border border-dashed bg-muted/20 px-6 py-10 text-center">
      <Inbox className="mb-3 size-8 text-muted-foreground" aria-hidden="true" />
      <h3 className="text-sm font-medium">{title}</h3>
      {description ? (
        <p className="mt-1 max-w-md text-sm text-muted-foreground">
          {description}
        </p>
      ) : null}
    </div>
  );
}
