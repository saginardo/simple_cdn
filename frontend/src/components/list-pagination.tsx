import type { ReactNode } from "react";
import { ChevronLeft, ChevronRight } from "lucide-react";
import { Button } from "@/components/ui/button";
import type { ListPaginationState } from "@/hooks/use-list-pagination";
import { cn } from "@/lib/utils";
import { t } from "@/lib/i18n";
type PaginationSummary = Omit<
  ListPaginationState<unknown>,
  "items" | "setPage"
>;
export function ListPagination({
  pagination,
  itemLabel = t("条记录"),
  disabled = false,
  action,
  className,
}: {
  pagination: PaginationSummary & {
    setPage: (page: number) => void;
  };
  itemLabel?: string;
  disabled?: boolean;
  action?: ReactNode;
  className?: string;
}) {
  const { page, totalPages, totalItems, start, end, setPage } = pagination;
  return (
    <nav
      className={cn(
        "flex min-h-11 flex-wrap items-center justify-between gap-2 border-t px-4 py-2.5 text-xs text-muted-foreground sm:px-5",
        className,
      )}
      aria-label={t("列表分页")}
    >
      <span className="tabular-nums">
        {t("第 ")}
        {start}-{end}
        {t(" 条，共 ")}
        {totalItems} {itemLabel}
      </span>
      <div className="flex items-center gap-1.5">
        <span className="mr-1 tabular-nums">
          {page} / {totalPages}
          {t(" 页")}
        </span>
        <Button
          type="button"
          variant="outline"
          size="icon-xs"
          aria-label={t("上一页")}
          disabled={disabled || page <= 1}
          onClick={() => setPage(page - 1)}
        >
          <ChevronLeft />
        </Button>
        <Button
          type="button"
          variant="outline"
          size="icon-xs"
          aria-label={t("下一页")}
          disabled={disabled || page >= totalPages}
          onClick={() => setPage(page + 1)}
        >
          <ChevronRight />
        </Button>
        {action ? <div className="ml-1 border-l pl-2">{action}</div> : null}
      </div>
    </nav>
  );
}
