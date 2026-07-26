import { useQuery } from "@tanstack/react-query";
import { Bell, ChevronRight } from "lucide-react";
import { Suspense, useEffect, useState } from "react";
import { Link, Outlet, useLocation } from "react-router";

import { AppSidebar } from "@/components/app-sidebar";
import { LanguageToggle } from "@/components/language-toggle";
import { MessageCenter, useMessages } from "@/components/message-center";
import { PageBody, PageLoading } from "@/components/page";
import { ThemeToggle } from "@/components/theme-toggle";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import {
  SidebarInset,
  SidebarProvider,
  SidebarTrigger,
} from "@/components/ui/sidebar";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { useAuth } from "@/features/auth/auth-provider";
import { cacheBranding, useCachedBranding } from "@/hooks/use-branding";
import { api } from "@/lib/api";
import { t, useI18n } from "@/lib/i18n";
import type { Settings, SystemInfo } from "@/lib/types";

const pageNames: Record<string, string> = {
  overview: "概览",
  logs: "日志",
  security: "安全",
  monitoring: "监测",
  scheduling: "调度",
  nodes: "节点",
  sites: "站点",
  certificates: "证书",
  settings: "设置",
};

export function AppShell() {
  useI18n();
  const [messagesOpen, setMessagesOpen] = useState(false);
  const { logout } = useAuth();
  const messageQuery = useMessages();
  const settingsQuery = useQuery({
    queryKey: ["settings"],
    queryFn: () => api<Settings>("/api/settings"),
  });
  const systemInfoQuery = useQuery({
    queryKey: ["system-info"],
    queryFn: () => api<SystemInfo>("/api/system/info"),
    staleTime: Infinity,
  });
  const cachedBranding = useCachedBranding();
  const branding = settingsQuery.data?.branding ?? cachedBranding;
  useEffect(() => {
    if (settingsQuery.data?.branding) {
      cacheBranding(settingsQuery.data.branding);
    }
  }, [settingsQuery.data?.branding]);
  const location = useLocation();
  const segments = location.pathname.split("/").filter(Boolean);
  const section = segments[0] || "overview";
  const detail = segments.length > 1;

  return (
    <SidebarProvider
      style={{ "--sidebar-width": "13.5rem" } as React.CSSProperties}
    >
      <AppSidebar
        brandName={branding?.name ?? ""}
        brandSubtitle={branding?.subtitle ?? ""}
        brandLogoDataURL={branding?.logo_data_url ?? ""}
        brandPending={!branding}
        productName={systemInfoQuery.data?.name ?? "simple_cdn"}
        productVersion={systemInfoQuery.data?.version ?? ""}
        onLogout={() => void logout()}
      />
      <SidebarInset className="min-w-0 bg-background">
        <header className="sticky top-0 z-20 flex h-14 shrink-0 items-center gap-2 border-b bg-background/95 px-3 backdrop-blur supports-[backdrop-filter]:bg-background/80 sm:px-5">
          <SidebarTrigger aria-label={t("切换侧边栏")} />
          <Separator orientation="vertical" className="mx-1 h-5" />
          <nav
            className="flex min-w-0 items-center gap-1 font-mono text-xs"
            aria-label={t("面包屑")}
          >
            <Link
              to={`/${section}`}
              className="truncate text-muted-foreground hover:text-foreground"
            >
              {t(pageNames[section] ?? "概览")}
            </Link>
            {detail ? (
              <>
                <ChevronRight className="size-3.5 shrink-0 text-muted-foreground" />
                <span className="truncate">{t("详情")}</span>
              </>
            ) : null}
          </nav>
          <div className="ml-auto flex items-center gap-1">
            <LanguageToggle />
            <ThemeToggle />
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon-sm"
                  className="relative"
                  aria-label={
                    messageQuery.data?.unread_count
                      ? t("打开消息中心，{count} 条未读", {
                          count: messageQuery.data.unread_count,
                        })
                      : t("打开消息中心")
                  }
                  onClick={() => setMessagesOpen(true)}
                >
                  <Bell />
                  {messageQuery.data?.unread_count ? (
                    <span
                      className="absolute -right-0.5 -top-0.5 size-2 rounded-full bg-destructive"
                      aria-hidden="true"
                    />
                  ) : null}
                </Button>
              </TooltipTrigger>
              <TooltipContent>{t("消息中心")}</TooltipContent>
            </Tooltip>
          </div>
        </header>
        <Suspense
          fallback={
            <PageBody>
              <PageLoading />
            </PageBody>
          }
        >
          <Outlet />
        </Suspense>
      </SidebarInset>
      <MessageCenter
        open={messagesOpen}
        onOpenChange={setMessagesOpen}
        page={messageQuery.data}
      />
    </SidebarProvider>
  );
}
