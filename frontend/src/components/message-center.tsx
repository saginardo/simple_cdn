import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { CheckCheck, Circle, Inbox, Trash2 } from "lucide-react";
import { useNavigate } from "react-router";
import { toast } from "sonner";
import { ListPagination } from "@/components/list-pagination";
import { Button } from "@/components/ui/button";
import { ScrollArea } from "@/components/ui/scroll-area";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { api, errorMessage } from "@/lib/api";
import { useListPagination } from "@/hooks/use-list-pagination";
import { usePersistentEnum } from "@/hooks/use-persistent-state";
import { formatDateTime } from "@/lib/format";
import { toneText, type Tone } from "@/lib/tones";
import type { Message, MessagePage } from "@/lib/types";
import { cn } from "@/lib/utils";
import { t } from "@/lib/i18n";
export function useMessages() {
  return useQuery({
    queryKey: ["messages"],
    queryFn: () => api<MessagePage>("/api/messages?limit=80"),
    refetchInterval: 10_000,
  });
}
export function MessageCenter({
  open,
  onOpenChange,
  page,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  page?: MessagePage;
}) {
  const [filter, setFilter] = usePersistentEnum(
    "simple-cdn.messages.filter",
    ["all", "unread"] as const,
    "all",
  );
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const invalidate = () =>
    queryClient.invalidateQueries({
      queryKey: ["messages"],
    });
  const markRead = useMutation({
    mutationFn: (id: string) =>
      api(`/api/messages/${encodeURIComponent(id)}/read`, {
        method: "POST",
        body: "{}",
      }),
    onSuccess: invalidate,
    onError: (error) => toast.error(errorMessage(error)),
  });
  const markAll = useMutation({
    mutationFn: () =>
      api("/api/messages/read-all", {
        method: "POST",
        body: "{}",
      }),
    onSuccess: invalidate,
    onError: (error) => toast.error(errorMessage(error)),
  });
  const remove = useMutation({
    mutationFn: (id: string) =>
      api(`/api/messages/${encodeURIComponent(id)}`, {
        method: "DELETE",
      }),
    onSuccess: invalidate,
    onError: (error) => toast.error(errorMessage(error)),
  });
  const messages = (page?.messages ?? []).filter(
    (message) => filter === "all" || !message.read_at,
  );
  const pagination = useListPagination(messages);
  function openMessage(message: Message) {
    if (!message.read_at) markRead.mutate(message.id);
    if (message.resource_type === "site" && message.resource_id)
      navigate(`/sites/${encodeURIComponent(message.resource_id)}`);
    else if (message.resource_type === "node" && message.resource_id)
      navigate(`/nodes/${encodeURIComponent(message.resource_id)}`);
    else if (message.category === "backup" || message.category === "restore")
      navigate("/settings");
    else return;
    onOpenChange(false);
  }
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="flex w-full flex-col gap-0 p-0 sm:max-w-md"
      >
        <SheetHeader className="border-b px-5 py-4 text-left">
          <div className="flex items-start justify-between gap-3 pr-8">
            <div>
              <SheetTitle>{t("消息中心")}</SheetTitle>
              <SheetDescription>
                {page?.unread_count ?? 0}
                {t(" 条未读")}
              </SheetDescription>
            </div>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="outline"
                  size="icon-sm"
                  disabled={!page?.unread_count || markAll.isPending}
                  onClick={() => markAll.mutate()}
                  aria-label={t("全部标为已读")}
                >
                  <CheckCheck />
                </Button>
              </TooltipTrigger>
              <TooltipContent>{t("全部标为已读")}</TooltipContent>
            </Tooltip>
          </div>
          <Tabs
            value={filter}
            onValueChange={(value) => {
              setFilter(value as typeof filter);
              pagination.setPage(1);
            }}
            className="mt-3"
          >
            <TabsList>
              <TabsTrigger value="all">{t("全部")}</TabsTrigger>
              <TabsTrigger value="unread">{t("未读")}</TabsTrigger>
            </TabsList>
          </Tabs>
        </SheetHeader>
        <ScrollArea className="min-h-0 flex-1">
          {messages.length ? (
            <div className="divide-y">
              {pagination.items.map((message) => (
                <article
                  key={message.id}
                  className={cn(
                    "group relative px-5 py-4 hover:bg-muted/40",
                    !message.read_at && "bg-info/5",
                  )}
                >
                  <button
                    type="button"
                    className="block w-full pr-8 text-left"
                    onClick={() => openMessage(message)}
                  >
                    <div className="mb-1 flex items-center gap-2">
                      {!message.read_at ? (
                        <Circle className="size-2 fill-info text-info" />
                      ) : null}
                      <span
                        className={cn(
                          "text-xs font-medium",
                          severityTone(message.severity),
                        )}
                      >
                        {severityLabel(message.severity)}
                      </span>
                      <time className="ml-auto text-xs text-muted-foreground">
                        {formatDateTime(message.created_at)}
                      </time>
                    </div>
                    <h3 className="text-sm font-medium">{message.title}</h3>
                    {message.body ? (
                      <p className="mt-1 line-clamp-3 text-sm leading-5 text-muted-foreground">
                        {message.body}
                      </p>
                    ) : null}
                  </button>
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    className="absolute right-4 top-10 opacity-0 group-hover:opacity-100 focus-visible:opacity-100"
                    aria-label={t("删除消息")}
                    onClick={() => remove.mutate(message.id)}
                  >
                    <Trash2 />
                  </Button>
                </article>
              ))}
            </div>
          ) : (
            <div className="grid min-h-72 place-items-center px-6 text-center text-sm text-muted-foreground">
              <div>
                <Inbox className="mx-auto mb-3 size-8" />
                <p>{t("暂无消息")}</p>
              </div>
            </div>
          )}
        </ScrollArea>
        {messages.length ? (
          <ListPagination
            pagination={pagination}
            itemLabel={t("条消息")}
            className="shrink-0"
          />
        ) : null}
      </SheetContent>
    </Sheet>
  );
}
function severityLabel(severity: Message["severity"]) {
  return {
    info: t("信息"),
    success: t("成功"),
    warning: t("注意"),
    error: t("失败"),
  }[severity];
}
const severityTones: Record<Message["severity"], Tone> = {
  info: "info",
  success: "success",
  warning: "warning",
  error: "danger",
};
function severityTone(severity: Message["severity"]) {
  return toneText[severityTones[severity]];
}
