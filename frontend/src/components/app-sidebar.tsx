import {
  Activity,
  BadgeCheck,
  LayoutDashboard,
  LogOut,
  Network,
  Route,
  ScrollText,
  Server,
  Settings,
  ShieldCheck,
  Waypoints,
} from "lucide-react";
import { Link, useLocation } from "react-router";

import { BrandMark } from "@/components/brand-mark";
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
  SidebarSeparator,
  useSidebar,
} from "@/components/ui/sidebar";
import { t } from "@/lib/i18n";

const groups = [
  {
    label: "工作区",
    items: [
      { label: "概览", to: "/overview", icon: LayoutDashboard },
      { label: "日志", to: "/logs", icon: ScrollText },
    ],
  },
  {
    label: "运营",
    items: [
      { label: "节点", to: "/nodes", icon: Server },
      { label: "站点", to: "/sites", icon: Waypoints },
      { label: "监测", to: "/monitoring", icon: Activity },
      { label: "调度", to: "/scheduling", icon: Route },
      { label: "WireGuard", to: "/wireguard", icon: Network },
      { label: "安全", to: "/security", icon: ShieldCheck },
      { label: "证书", to: "/certificates", icon: BadgeCheck },
    ],
  },
  {
    label: "系统",
    items: [{ label: "设置", to: "/settings", icon: Settings }],
  },
];

export function AppSidebar({
  brandName,
  brandSubtitle,
  brandLogoDataURL,
  brandPending,
  productName,
  productVersion,
  onLogout,
}: {
  brandName: string;
  brandSubtitle: string;
  brandLogoDataURL: string;
  brandPending?: boolean;
  productName: string;
  productVersion: string;
  onLogout: () => void;
}) {
  const location = useLocation();
  const { isMobile, setOpenMobile } = useSidebar();
  const versionLabel = productVersion.startsWith("v")
    ? productVersion
    : `v${productVersion}`;
  return (
    <Sidebar collapsible="icon" className="bg-sidebar">
      <SidebarHeader className="px-3 py-4">
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton
              size="lg"
              tooltip={brandName || t("控制面板")}
              className="h-12 justify-start rounded-lg px-2"
            >
              <BrandMark logoDataURL={brandLogoDataURL} className="size-9" />
              {brandPending ? (
                <span
                  className="grid min-w-0 gap-1.5"
                  aria-label={t("正在加载品牌")}
                >
                  <span className="h-3 w-24 bg-sidebar-accent" />
                  <span className="h-2.5 w-16 bg-sidebar-accent" />
                </span>
              ) : (
                <span className="grid min-w-0 text-left leading-tight">
                  <span className="truncate font-semibold">{brandName}</span>
                  {brandSubtitle ? (
                    <span className="truncate text-xs text-muted-foreground">
                      {t(brandSubtitle)}
                    </span>
                  ) : null}
                </span>
              )}
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>
      <SidebarSeparator />
      <SidebarContent>
        {groups.map((group) => (
          <SidebarGroup key={group.label} className="px-2 py-2.5">
            <SidebarGroupLabel className="h-8 justify-start px-2.5 text-[0.8125rem] font-semibold text-sidebar-foreground/80">
              {t(group.label)}
            </SidebarGroupLabel>
            <SidebarGroupContent>
              <SidebarMenu>
                {group.items.map((item) => {
                  const active =
                    location.pathname === item.to ||
                    location.pathname.startsWith(`${item.to}/`);
                  return (
                    <SidebarMenuItem key={item.to}>
                      <SidebarMenuButton
                        asChild
                        tooltip={t(item.label)}
                        isActive={active}
                        className="relative h-9 justify-start rounded-md px-2.5 text-[0.8125rem] data-[active=true]:before:absolute data-[active=true]:before:inset-y-2 data-[active=true]:before:left-0 data-[active=true]:before:w-0.5 data-[active=true]:before:rounded-r data-[active=true]:before:bg-sidebar-primary group-data-[collapsible=icon]:data-[active=true]:before:left-0.5"
                      >
                        <Link
                          to={item.to}
                          onClick={() => {
                            if (isMobile) setOpenMobile(false);
                          }}
                        >
                          <item.icon />
                          <span>{t(item.label)}</span>
                        </Link>
                      </SidebarMenuButton>
                    </SidebarMenuItem>
                  );
                })}
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
        ))}
      </SidebarContent>
      <SidebarSeparator />
      <SidebarFooter>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton
              tooltip={t("退出登录")}
              onClick={onLogout}
              className="h-9 justify-start rounded-md px-2.5 text-muted-foreground hover:text-foreground"
            >
              <LogOut />
              <span>{t("退出登录")}</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
        {productVersion ? (
          <div
            aria-label={`${productName} ${t("版本")} ${versionLabel}`}
            className="flex h-7 min-w-0 items-center justify-between gap-2 px-2.5 font-mono text-[11px] leading-none text-sidebar-foreground/50 group-data-[collapsible=icon]:hidden"
          >
            <span className="truncate">{productName}</span>
            <span className="shrink-0 font-mono tabular-nums">
              {versionLabel}
            </span>
          </div>
        ) : null}
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  );
}
