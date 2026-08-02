import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ThemeProvider } from "next-themes";
import { lazy } from "react";
import { HashRouter, Navigate, Route, Routes } from "react-router";

import { AppShell } from "@/components/app-shell";
import { BrandingSync } from "@/components/branding-sync";
import { TooltipProvider } from "@/components/ui/tooltip";
import { Toaster } from "@/components/ui/sonner";
import { AuthProvider, AuthGate } from "@/features/auth/auth-provider";
import { I18nProvider } from "@/lib/i18n";

const OverviewPage = lazy(() =>
  import("@/features/overview/overview-page").then((module) => ({
    default: module.OverviewPage,
  })),
);
const OverviewSitePage = lazy(() =>
  import("@/features/overview/overview-site-page").then((module) => ({
    default: module.OverviewSitePage,
  })),
);
const LogsPage = lazy(() =>
  import("@/features/logs/logs-page").then((module) => ({
    default: module.LogsPage,
  })),
);
const LogDetailPage = lazy(() =>
  import("@/features/logs/log-detail-page").then((module) => ({
    default: module.LogDetailPage,
  })),
);
const SecurityPage = lazy(() =>
  import("@/features/security/security-page").then((module) => ({
    default: module.SecurityPage,
  })),
);
const StaticAssetsPage = lazy(() =>
  import("@/features/static-assets/static-assets-page").then((module) => ({
    default: module.StaticAssetsPage,
  })),
);
const MonitoringPage = lazy(() =>
  import("@/features/monitoring/monitoring-page").then((module) => ({
    default: module.MonitoringPage,
  })),
);
const MonitoringNodeHistoryPage = lazy(() =>
  import("@/features/monitoring/monitoring-node-history-page").then(
    (module) => ({ default: module.MonitoringNodeHistoryPage }),
  ),
);
const SchedulingPage = lazy(() =>
  import("@/features/scheduling/scheduling-page").then((module) => ({
    default: module.SchedulingPage,
  })),
);
const WireGuardPage = lazy(() =>
  import("@/features/wireguard/wireguard-page").then((module) => ({
    default: module.WireGuardPage,
  })),
);
const WireGuardDetailPage = lazy(() =>
  import("@/features/wireguard/wireguard-detail-page").then((module) => ({
    default: module.WireGuardDetailPage,
  })),
);
const NodesPage = lazy(() =>
  import("@/features/nodes/nodes-page").then((module) => ({
    default: module.NodesPage,
  })),
);
const NodeDetailPage = lazy(() =>
  import("@/features/nodes/node-detail-page").then((module) => ({
    default: module.NodeDetailPage,
  })),
);
const SitesPage = lazy(() =>
  import("@/features/sites/sites-page").then((module) => ({
    default: module.SitesPage,
  })),
);
const CertificatesPage = lazy(() =>
  import("@/features/certificates/certificates-page").then((module) => ({
    default: module.CertificatesPage,
  })),
);
const SiteDetailPage = lazy(() =>
  import("@/features/sites/site-detail-page").then((module) => ({
    default: module.SiteDetailPage,
  })),
);
const SettingsPage = lazy(() =>
  import("@/features/settings/settings-page").then((module) => ({
    default: module.SettingsPage,
  })),
);

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 10_000,
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});

export default function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <BrandingSync />
      <ThemeProvider
        attribute="class"
        defaultTheme="system"
        enableSystem
        disableTransitionOnChange
      >
        <TooltipProvider delayDuration={300}>
          <I18nProvider>
            <AuthProvider>
              <AuthGate>
                <HashRouter>
                  <Routes>
                    <Route element={<AppShell />}>
                      <Route
                        index
                        element={<Navigate to="/overview" replace />}
                      />
                      <Route path="/overview" element={<OverviewPage />} />
                      <Route
                        path="/overview/sites/:siteId"
                        element={<OverviewSitePage />}
                      />
                      <Route path="/logs" element={<LogsPage />} />
                      <Route path="/logs/:logId" element={<LogDetailPage />} />
                      <Route path="/security" element={<SecurityPage />} />
                      <Route
                        path="/static-assets"
                        element={<StaticAssetsPage />}
                      />
                      <Route path="/monitoring" element={<MonitoringPage />} />
                      <Route path="/scheduling" element={<SchedulingPage />} />
                      <Route path="/wireguard" element={<WireGuardPage />} />
                      <Route
                        path="/wireguard/:tunnelId"
                        element={<WireGuardDetailPage />}
                      />
                      <Route
                        path="/monitoring/nodes/:nodeId"
                        element={<MonitoringNodeHistoryPage />}
                      />
                      <Route path="/nodes" element={<NodesPage />} />
                      <Route
                        path="/nodes/:nodeId"
                        element={<NodeDetailPage />}
                      />
                      <Route path="/sites" element={<SitesPage />} />
                      <Route path="/sites/new" element={<SiteDetailPage />} />
                      <Route
                        path="/sites/:siteId"
                        element={<SiteDetailPage />}
                      />
                      <Route
                        path="/certificates"
                        element={<CertificatesPage />}
                      />
                      <Route path="/settings" element={<SettingsPage />} />
                      <Route
                        path="*"
                        element={<Navigate to="/overview" replace />}
                      />
                    </Route>
                  </Routes>
                </HashRouter>
              </AuthGate>
            </AuthProvider>
            <Toaster position="top-right" richColors closeButton />
          </I18nProvider>
        </TooltipProvider>
      </ThemeProvider>
    </QueryClientProvider>
  );
}
