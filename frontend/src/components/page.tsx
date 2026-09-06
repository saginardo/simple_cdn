import type { ReactNode } from "react";
import { AlertCircle, Inbox, RotateCcw } from "lucide-react";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
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
    <header className="flex min-w-0 flex-col gap-4 px-4 pb-2 pt-6 sm:flex-row sm:items-center sm:justify-between sm:px-6 lg:px-8 lg:pt-7">
      <div className="min-w-0">
        <h1 className="break-words text-2xl font-semibold leading-tight tracking-normal">
          {title}
        </h1>
        {description ? (
          <p className="mt-1.5 text-xs leading-relaxed text-muted-foreground">
            {description}
          </p>
        ) : null}
      </div>
      {actions ? (
        <div className="flex min-w-0 max-w-full flex-wrap items-center gap-2 sm:justify-end">
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
        "min-w-0 flex-1 space-y-5 p-4 sm:p-6 lg:px-8 lg:pb-8",
        className,
      )}
    >
      {children}
    </div>
  );
}

/**
 * Full-width data surface, separated from the canvas by horizontal rules.
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
        "min-w-0 overflow-hidden border-y bg-card text-sm text-card-foreground",
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
  onRetry,
}: {
  title?: string;
  error: unknown;
  onRetry?: () => void;
}) {
  return (
    <Alert variant="destructive">
      <AlertCircle />
      <AlertTitle>{title}</AlertTitle>
      <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
        <span className="min-w-0 break-words">
          {error instanceof Error ? error.message : t("发生未知错误")}
        </span>
        {onRetry ? (
          <Button type="button" variant="outline" size="sm" onClick={onRetry}>
            <RotateCcw />
            {t("重试")}
          </Button>
        ) : null}
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
    <div className="flex min-h-48 flex-col items-center justify-center px-6 py-10 text-center">
      <div className="mb-3.5 flex size-10 items-center justify-center rounded-lg bg-muted text-muted-foreground">
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
