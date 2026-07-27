import { expect, test, type Page } from "@playwright/test";

const now = new Date("2026-07-18T10:00:00Z");
const series = Array.from({ length: 24 }, (_, index) => ({
  time: new Date(now.getTime() - (23 - index) * 60 * 60 * 1000).toISOString(),
  requests: 900 + index * 57 + (index % 4) * 160,
  bytes: 72_000_000 + index * 4_200_000,
  error_requests: 12 + (index % 5) * 6,
}));

const overview = {
  from: series[0].time,
  to: now.toISOString(),
  bucket_seconds: 3600,
  totals: { requests: 38241, bytes: 3_948_238_121, error_requests: 612 },
  series,
  status_codes: [
    { code: 200, requests: 34120 },
    { code: 304, requests: 2100 },
    { code: 404, requests: 1240 },
    { code: 502, requests: 781 },
  ],
  sites: [
    {
      id: "site-1",
      name: "静态资源主站",
      domains: ["cdn.example.com", "static.example.com"],
      requests: 28130,
      bytes: 3_122_000_000,
      error_requests: 342,
      status_codes: [
        { code: 200, requests: 27100 },
        { code: 404, requests: 1030 },
      ],
      series,
    },
    {
      id: "site-2",
      name: "API 加速",
      domains: ["api.example.com"],
      requests: 10111,
      bytes: 826_238_121,
      error_requests: 270,
      status_codes: [
        { code: 200, requests: 9330 },
        { code: 502, requests: 781 },
      ],
      series: series.map((point) => ({
        ...point,
        requests: Math.round(point.requests / 3),
        bytes: Math.round(point.bytes / 4),
      })),
    },
  ],
};

const site = {
  id: "site-1",
  name: "静态资源主站",
  zone_id: "zone-1",
  domains: ["cdn.example.com"],
  node_ids: [],
  primary_origin: {
    url: "https://origin.example.com",
    host_header: "origin.example.com",
    tls_server_name: "origin.example.com",
    enabled: true,
  },
  stream_paths: [],
  passthrough: false,
  http3_enabled: false,
  client_max_body_size_mb: 128,
  client_keepalive_timeout_seconds: 120,
  read_write_timeout_seconds: 360,
  dns_ttl_seconds: null,
  tcp_only: false,
  tcp_forwards: [],
  cache_generation: 2,
  config_version: 8,
  published: true,
  enabled: true,
  deleting: false,
  created_at: now.toISOString(),
  updated_at: now.toISOString(),
};

const certificateOverview = {
  renewal_window_days: 30,
  reconcile_interval_seconds: 43_200,
  sites: [
    {
      site_id: "site-1",
      site_name: "静态资源主站",
      domains: ["cdn.example.com"],
      enabled: true,
      published: true,
      deleting: false,
      needs_certificate: true,
      certificate_present: true,
      certificate_updated_at: now.toISOString(),
      not_after: new Date(now.getTime() + 60 * 86_400_000).toISOString(),
      renewal_due_at: new Date(now.getTime() + 30 * 86_400_000).toISOString(),
      published_after_certificate: true,
      task: null,
    },
    {
      site_id: "site-2",
      site_name: "临近续期 API",
      domains: ["api.example.com"],
      enabled: true,
      published: true,
      deleting: false,
      needs_certificate: true,
      certificate_present: true,
      certificate_updated_at: series[20].time,
      not_after: new Date(now.getTime() + 10 * 86_400_000).toISOString(),
      renewal_due_at: new Date(now.getTime() - 20 * 86_400_000).toISOString(),
      published_after_certificate: false,
      task: null,
    },
    {
      site_id: "site-3",
      site_name: "尚未签发站点",
      domains: ["new.example.com"],
      enabled: true,
      published: false,
      deleting: false,
      needs_certificate: true,
      certificate_present: false,
      published_after_certificate: false,
      task: null,
    },
    {
      site_id: "site-4",
      site_name: "明文 TCP 入口",
      domains: ["tcp.example.com"],
      enabled: true,
      published: true,
      deleting: false,
      needs_certificate: false,
      certificate_present: false,
      published_after_certificate: false,
      task: null,
    },
  ],
};

const monitoring = {
  interval_seconds: 30,
  attempts_per_round: 3,
  healthy_score: 80,
  auto_pause_after: 4,
  targets: [
    {
      id: "target-1",
      name: "主 API",
      address: "probe-a.example.com:443",
      enabled: true,
      created_at: series[20].time,
      updated_at: series[20].time,
    },
    {
      id: "target-2",
      name: "备用入口",
      address: "192.0.2.50:8443",
      enabled: true,
      created_at: series[20].time,
      updated_at: series[21].time,
    },
  ],
  nodes: [
    {
      node_id: "node-1",
      name: "edge-hong-kong",
      public_ipv4: "203.0.113.41",
      status: "active",
      monitor_auto_paused: false,
      capable: true,
      score: 96,
      success_rate: 100,
      average_latency_ms: 63.4,
      consecutive_abnormal: 0,
      last_checked_at: now.toISOString(),
      stale: false,
      results: [
        {
          target_id: "target-1",
          target_name: "主 API",
          address: "probe-a.example.com:443",
          attempts: 3,
          successful_attempts: 3,
          average_latency_ms: 58.2,
          checked_at: now.toISOString(),
        },
        {
          target_id: "target-2",
          target_name: "备用入口",
          address: "192.0.2.50:8443",
          attempts: 3,
          successful_attempts: 3,
          average_latency_ms: 68.6,
          checked_at: now.toISOString(),
        },
      ],
    },
    {
      node_id: "node-2",
      name: "edge-singapore",
      public_ipv4: "203.0.113.42",
      status: "draining",
      monitor_auto_paused: true,
      capable: true,
      score: 35,
      success_rate: 50,
      average_latency_ms: 1320,
      consecutive_abnormal: 4,
      last_checked_at: now.toISOString(),
      stale: false,
      results: [
        {
          target_id: "target-1",
          target_name: "主 API",
          address: "probe-a.example.com:443",
          attempts: 3,
          successful_attempts: 3,
          average_latency_ms: 1320,
          checked_at: now.toISOString(),
        },
        {
          target_id: "target-2",
          target_name: "备用入口",
          address: "192.0.2.50:8443",
          attempts: 3,
          successful_attempts: 0,
          average_latency_ms: 0,
          error: "connect: connection timed out",
          checked_at: now.toISOString(),
        },
      ],
    },
  ],
};

const smartRouting = {
  timezone: "Asia/Shanghai",
  nodes: [
    {
      node_id: "node-1",
      name: "edge-hong-kong",
      public_ipv4: "203.0.113.41",
      status: "active",
      capable: true,
      auto_paused: false,
      enabled: true,
      blocked_by: [],
      score: {
        enabled: true,
        pause_below_score: 80,
        pause_after_rounds: 4,
        resume_at_score: 85,
        resume_after_rounds: 3,
        gate: "allowed",
        current_score: 96,
        last_checked_at: now.toISOString(),
        stale: false,
        low_streak: 0,
        recovery_streak: 0,
      },
      schedule: {
        enabled: true,
        windows: [
          {
            weekdays: [1, 2, 3, 4, 5, 6, 7],
            start: "08:00",
            end: "23:00",
          },
        ],
        gate: "open",
        next_transition_at: new Date(
          now.getTime() + 5 * 60 * 60 * 1000,
        ).toISOString(),
      },
      updated_at: now.toISOString(),
    },
    {
      node_id: "node-2",
      name: "edge-singapore",
      public_ipv4: "203.0.113.42",
      status: "draining",
      capable: true,
      auto_paused: true,
      enabled: true,
      blocked_by: ["score", "schedule"],
      score: {
        enabled: true,
        pause_below_score: 80,
        pause_after_rounds: 4,
        resume_at_score: 85,
        resume_after_rounds: 3,
        gate: "blocked",
        current_score: 35,
        last_checked_at: now.toISOString(),
        stale: false,
        low_streak: 0,
        recovery_streak: 0,
      },
      schedule: {
        enabled: true,
        windows: [{ weekdays: [1, 2, 3, 4, 5], start: "09:00", end: "18:00" }],
        gate: "closed",
        next_transition_at: new Date(
          now.getTime() + 2 * 24 * 60 * 60 * 1000,
        ).toISOString(),
      },
      updated_at: now.toISOString(),
    },
  ],
};

function monitoringHistory(range: string) {
  const presets: Record<string, { duration: number; bucket: number }> = {
    "1h": { duration: 60 * 60 * 1000, bucket: 30 },
    "6h": { duration: 6 * 60 * 60 * 1000, bucket: 120 },
    "12h": { duration: 12 * 60 * 60 * 1000, bucket: 300 },
    "24h": { duration: 24 * 60 * 60 * 1000, bucket: 600 },
    "7d": { duration: 7 * 24 * 60 * 60 * 1000, bucket: 3600 },
  };
  const selectedRange = presets[range] ? range : "24h";
  const preset = presets[selectedRange];
  const points = Array.from({ length: 16 }, (_, index) => {
    const time = new Date(
      now.getTime() - preset.duration + (preset.duration * index) / 15,
    ).toISOString();
    return { time, index };
  });
  return {
    available: true,
    node: {
      id: "node-1",
      name: "edge-hong-kong",
      public_ipv4: "203.0.113.41",
      status: "active",
      monitor_auto_paused: false,
    },
    range: selectedRange,
    from: new Date(now.getTime() - preset.duration).toISOString(),
    to: now.toISOString(),
    bucket_seconds: preset.bucket,
    series: [
      {
        target_id: "target-1",
        name: "主 API",
        address: "probe-a.example.com:443",
        points: points.map(({ time, index }) => ({
          time,
          attempts: 3,
          successful_attempts: 3,
          success_rate: 100,
          average_latency_ms: 42 + (index % 5) * 3,
          failed_rounds: 0,
        })),
      },
      {
        target_id: "target-2",
        name: "备用入口",
        address: "192.0.2.50:8443",
        points: points.map(({ time, index }) => ({
          time,
          attempts: 3,
          successful_attempts: index === 8 ? 0 : 3,
          success_rate: index === 8 ? 0 : 100,
          average_latency_ms: index === 8 ? null : 71 + (index % 4) * 4,
          failed_rounds: index === 8 ? 1 : 0,
        })),
      },
    ],
  };
}

const accessLogs = [
  {
    id: "request-404",
    timestamp: now.toISOString(),
    node_id: "node-1",
    site_id: "site-1",
    client_ip: "203.0.113.25",
    host: "cdn.example.com",
    scheme: "https",
    protocol: "HTTP/2.0",
    method: "GET",
    path: "/assets/releases/2026/07/18/a-very-long-directory-name/another-very-long-directory-name/application.bundle.js",
    status: 404,
    request_bytes: 2048,
    bytes: 8192,
    duration_ms: 37,
    upstream: "192.0.2.10:443",
    upstream_status: "404",
    upstream_response_time: "0.036",
    cache_status: "MISS",
    user_agent: "Mozilla/5.0 (Playwright request detail test)",
    referer: "https://cdn.example.com/releases",
    content_type: "application/json",
    response_content_type: "text/html; charset=utf-8",
    accept: "text/html,application/xhtml+xml",
    range: "bytes=0-4095",
  },
  {
    id: "request-502",
    timestamp: series[22].time,
    node_id: "node-1",
    site_id: "site-1",
    client_ip: "203.0.113.26",
    host: "cdn.example.com",
    scheme: "https",
    protocol: "HTTP/2.0",
    method: "GET",
    path: "/api/unavailable",
    status: 502,
    request_bytes: 512,
    bytes: 128,
    duration_ms: 1001,
    upstream: "192.0.2.10:443",
    upstream_status: "502",
    upstream_response_time: "1.000",
    cache_status: "MISS",
    user_agent: "curl/8.10.1",
    referer: "",
    content_type: "",
    response_content_type: "text/plain",
    accept: "*/*",
    range: "",
  },
];

async function mockAPI(page: Page, overrides: Record<string, unknown> = {}) {
  let branding = {
    name: "simple_cdn",
    subtitle: "控制面板",
    logo_data_url: "",
  };
  let cacheDefaultSizeGB = 1;
  let nodeCacheOverrideGB: number | null = null;
  const deliveredMachineEvents = new Set<string>();
  let backupSnapshots = Array.isArray(overrides["/api/backups/snapshots"])
    ? [...overrides["/api/backups/snapshots"]]
    : [];
  await page.route("**/api/**", async (route) => {
    const url = new URL(route.request().url());
    if (/^\/api\/nodes\/[^/]+\/machine-status\/events$/.test(url.pathname)) {
      const event = overrides[url.pathname];
      if (event === undefined || deliveredMachineEvents.has(url.pathname)) {
        await route.fulfill({ status: 204 });
        return;
      }
      deliveredMachineEvents.add(url.pathname);
      await route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        headers: { "cache-control": "no-store" },
        body: `event: machine-status\ndata: ${JSON.stringify(event)}\n\n`,
      });
      return;
    }
    const snapshotDeleteMatch = url.pathname.match(
      /^\/api\/backups\/snapshots\/([^/]+)$/,
    );
    if (snapshotDeleteMatch && route.request().method() === "DELETE") {
      const snapshotID = decodeURIComponent(snapshotDeleteMatch[1]);
      const snapshot = backupSnapshots.find(
        (item) =>
          typeof item === "object" &&
          item !== null &&
          "id" in item &&
          item.id === snapshotID,
      ) as { id: string; short_id?: string } | undefined;
      const input = route.request().postDataJSON() as {
        confirmation?: string;
      };
      if (!snapshot) {
        await route.fulfill({
          status: 404,
          contentType: "application/json",
          body: JSON.stringify({ error: "backup snapshot was not found" }),
        });
        return;
      }
      if (input.confirmation !== snapshot.short_id) {
        await route.fulfill({
          status: 400,
          contentType: "application/json",
          body: JSON.stringify({
            error: "confirmation must match the snapshot short ID",
          }),
        });
        return;
      }
      backupSnapshots = backupSnapshots.filter(
        (item) =>
          typeof item !== "object" ||
          item === null ||
          !("id" in item) ||
          item.id !== snapshotID,
      );
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ deleted_snapshot_id: snapshotID }),
      });
      return;
    }
    if (
      route.request().method() === "POST" &&
      (url.pathname.match(/^\/api\/certificates\/[^/]+\/renew$/) ||
        url.pathname.match(/^\/api\/sites\/[^/]+\/certificate$/))
    ) {
      const siteID = url.pathname.split("/")[3];
      await route.fulfill({
        status: 202,
        contentType: "application/json",
        body: JSON.stringify({
          id: "certificate-task-new",
          kind: url.pathname.endsWith("renew")
            ? "renew_certificate"
            : "issue_certificate",
          site_id: siteID,
          status: "queued",
          created_at: now.toISOString(),
          updated_at: now.toISOString(),
        }),
      });
      return;
    }
    if (url.pathname === "/api/sites" && route.request().method() === "POST") {
      const input = route.request().postDataJSON() as Record<string, unknown>;
      await route.fulfill({
        status: 201,
        contentType: "application/json",
        body: JSON.stringify({
          ...site,
          ...input,
          id: "site-created",
          zone_id: "zone-resolved",
          published: false,
          created_at: now.toISOString(),
          updated_at: now.toISOString(),
        }),
      });
      return;
    }
    if (
      url.pathname === "/api/settings/branding" &&
      route.request().method() === "PUT"
    ) {
      branding = {
        ...branding,
        ...(route.request().postDataJSON() as Partial<typeof branding>),
      };
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(branding),
      });
      return;
    }
    if (
      url.pathname === "/api/nodes/node-1/cache" &&
      route.request().method() === "PUT"
    ) {
      const input = route.request().postDataJSON() as {
        cache_max_size_gb: number | null;
      };
      nodeCacheOverrideGB = input.cache_max_size_gb;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          default_size_gb: cacheDefaultSizeGB,
          override_size_gb: nodeCacheOverrideGB,
          effective_size_gb: nodeCacheOverrideGB ?? cacheDefaultSizeGB,
        }),
      });
      return;
    }
    if (
      url.pathname === "/api/nodes/node-1/status" &&
      route.request().method() === "POST"
    ) {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true, smart_routing_disabled: true }),
      });
      return;
    }
    if (
      url.pathname === "/api/settings/cache" &&
      route.request().method() === "PUT"
    ) {
      const input = route.request().postDataJSON() as {
        default_size_gb: number;
      };
      cacheDefaultSizeGB = input.default_size_gb;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ default_size_gb: cacheDefaultSizeGB }),
      });
      return;
    }
    if (
      url.pathname === "/api/monitoring/targets" &&
      route.request().method() === "POST"
    ) {
      const input = route.request().postDataJSON() as {
        name: string;
        address: string;
      };
      await route.fulfill({
        status: 201,
        contentType: "application/json",
        body: JSON.stringify({
          id: "target-created",
          name: input.name,
          address: input.address,
          enabled: true,
          created_at: now.toISOString(),
          updated_at: now.toISOString(),
        }),
      });
      return;
    }
    if (
      url.pathname.match(/^\/api\/monitoring\/nodes\/[^/]+\/smart-routing$/) &&
      route.request().method() === "PUT"
    ) {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true }),
      });
      return;
    }
    if (
      url.pathname.startsWith("/api/monitoring/targets/") &&
      route.request().method() === "PUT"
    ) {
      const input = route.request().postDataJSON() as {
        name?: string;
        enabled?: boolean;
      };
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          id: url.pathname.split("/").at(-1),
          name: input.name ?? "主 API",
          address: "probe-a.example.com:443",
          enabled: input.enabled ?? true,
          created_at: now.toISOString(),
          updated_at: now.toISOString(),
        }),
      });
      return;
    }
    if (
      url.pathname === "/api/monitoring/nodes/node-1/history" &&
      route.request().method() === "GET"
    ) {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(
          monitoringHistory(url.searchParams.get("range") ?? "24h"),
        ),
      });
      return;
    }
    if (
      url.pathname.startsWith("/api/monitoring/targets/") &&
      route.request().method() === "DELETE"
    ) {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ ok: true }),
      });
      return;
    }
    const responses: Record<string, unknown> = {
      "/api/session": { user: "admin", csrf_token: "e2e-csrf" },
      "/api/system/info": { name: "simple_cdn", version: "0.1.1" },
      "/api/branding": branding,
      "/api/messages": { messages: [], unread_count: 0 },
      "/api/overview": overview,
      "/api/certificates": certificateOverview,
      "/api/sites": [site],
      "/api/sites/site-1/publish-status": {
        task: {
          id: "publish-1",
          kind: "publish",
          site_id: "site-1",
          status: "succeeded",
          created_at: now.toISOString(),
          updated_at: now.toISOString(),
        },
        nodes: [],
      },
      "/api/sites/site-1/tls-status": {
        certificate_task: {
          id: "tls-1",
          kind: "issue_certificate",
          site_id: "site-1",
          status: "succeeded",
          created_at: now.toISOString(),
          updated_at: now.toISOString(),
        },
        published_after_certificate: true,
      },
      "/api/nodes": [],
      "/api/logs": {
        logs: accessLogs,
        from: series[22].time,
        to: now.toISOString(),
        offset: 0,
        page_size: 20,
        has_more: false,
      },
      "/api/logs/request-404": accessLogs[0],
      "/api/logs/request-502": accessLogs[1],
      "/api/security": {
        policies: [],
        rate_limit_policies: [],
        bans: [],
        active_ban_count: 0,
        events: [],
        nodes: [],
      },
      "/api/monitoring": monitoring,
      "/api/monitoring/smart-routing": smartRouting,
      "/api/settings": {
        branding,
        cache: { default_size_gb: cacheDefaultSizeGB },
        dns: { default_ttl_seconds: 60 },
        cloudflare: {
          source: "environment",
          configured: true,
          override_configured: false,
          environment_configured: true,
        },
        smtp: {
          enabled: false,
          host: "",
          port: 587,
          username: "",
          from_address: "",
          recipients: [],
          notification_categories: [
            "availability",
            "monitoring",
            "certificate",
            "backup",
          ],
          security: "starttls",
          source: "unconfigured",
          override_configured: false,
          password_configured: false,
          environment_configured: false,
        },
        backup: {
          repository: "",
          access_key_id: "",
          region: "us-east-1",
          backup_time: "03:25",
          random_delay_seconds: 1200,
          source: "unconfigured",
          configured: false,
          override_configured: false,
          secret_access_key_configured: false,
          restic_password_configured: false,
          environment_configured: false,
        },
      },
      "/api/backups/status": null,
      "/api/backups/snapshots": [],
      "/api/backups/restores/current": null,
      ...overrides,
    };
    let data =
      url.pathname === "/api/backups/snapshots"
        ? backupSnapshots
        : responses[url.pathname];
    if (
      url.pathname === "/api/nodes/node-1" &&
      data &&
      typeof data === "object"
    ) {
      data = {
        ...(data as Record<string, unknown>),
        cache: {
          default_size_gb: cacheDefaultSizeGB,
          override_size_gb: nodeCacheOverrideGB,
          effective_size_gb: nodeCacheOverrideGB ?? cacheDefaultSizeGB,
        },
      };
    }
    if (data === undefined) {
      await route.fulfill({
        status: 404,
        contentType: "application/json",
        body: JSON.stringify({ error: "not mocked" }),
      });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(data),
    });
  });
}

test("desktop overview renders shadcn chart and aligned navigation", async ({
  page,
}, testInfo) => {
  const errors = trackPageErrors(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  await mockAPI(page);
  await page.goto("/#/overview");

  await expect(
    page.getByRole("heading", { name: "概览", level: 1 }),
  ).toBeVisible();
  await expect(page.getByText("38,241", { exact: true })).toBeVisible();
  await expect(page.getByText("静态资源主站")).toBeVisible();
  await expect(page.getByLabel("simple_cdn 版本 v0.1.1")).toBeVisible();
  const metricBand = page.locator('[data-slot="metric-band"]');
  await expect(metricBand).toBeVisible();
  await expect(
    metricBand.locator('[data-slot="metric-band-item"]'),
  ).toHaveCount(4);
  const chart = page.locator('[data-slot="chart"] svg').first();
  await expect(chart).toBeVisible();
  expect((await chart.boundingBox())?.height).toBeGreaterThan(200);
  await expect(chart.locator("path.recharts-line-curve")).toHaveCount(1);
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  expect(errors).toEqual([]);

  await page.screenshot({
    path: testInfo.outputPath("overview-desktop.png"),
    fullPage: true,
  });
});

test("mobile overview keeps metric controls within the viewport", async ({
  page,
}, testInfo) => {
  const errors = trackPageErrors(page);
  await page.setViewportSize({ width: 390, height: 844 });
  await mockAPI(page);
  await page.goto("/#/overview");

  await expect(
    page.getByRole("heading", { name: "概览", level: 1 }),
  ).toBeVisible();
  await expect(page.getByLabel("关键指标")).toBeVisible();
  const tabs = page.locator('[data-slot="tabs-list"]');
  await expect(tabs).toBeVisible();
  expect(
    await tabs.evaluate(
      (element) => element.scrollWidth <= element.clientWidth + 1,
    ),
  ).toBe(true);
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  expect(errors).toEqual([]);

  await page.screenshot({
    path: testInfo.outputPath("overview-mobile.png"),
    fullPage: true,
  });
});

test("list pagination renders at most 20 entries per page", async ({
  page,
}) => {
  const sites = Array.from({ length: 25 }, (_, index) => ({
    ...overview.sites[0],
    id: `site-${index + 1}`,
    name: `分页站点 ${String(index + 1).padStart(2, "0")}`,
  }));
  await mockAPI(page, { "/api/overview": { ...overview, sites } });
  await page.goto("/#/overview");

  const rows = page.locator("tbody tr");
  await expect(rows).toHaveCount(20);
  await expect(page.getByText("第 1-20 条，共 25 个站点")).toBeVisible();
  await expect(page.getByText("分页站点 20")).toBeVisible();
  await expect(page.getByText("分页站点 21")).toHaveCount(0);

  await page.getByRole("button", { name: "下一页" }).click();
  await expect(rows).toHaveCount(5);
  await expect(page.getByText("第 21-25 条，共 25 个站点")).toBeVisible();
  await expect(page.getByText("分页站点 21")).toBeVisible();
  await expect(page.getByText("分页站点 20")).toHaveCount(0);
});

test("overview site traffic sorts by the selected column", async ({ page }) => {
  const sites = [
    {
      ...overview.sites[0],
      id: "site-alpha",
      name: "Alpha",
      requests: 10,
      bytes: 300,
    },
    {
      ...overview.sites[0],
      id: "site-bravo",
      name: "Bravo",
      requests: 30,
      bytes: 100,
    },
    {
      ...overview.sites[0],
      id: "site-charlie",
      name: "Charlie",
      requests: 20,
      bytes: 200,
    },
  ];
  await mockAPI(page, { "/api/overview": { ...overview, sites } });
  await page.goto("/#/overview");

  const table = page.getByRole("table");
  const firstRow = table.locator("tbody tr").first();
  const requestsHeader = page.getByRole("columnheader", { name: "请求数" });

  await expect(requestsHeader).toHaveAttribute("aria-sort", "descending");
  await expect(firstRow).toContainText("Bravo");

  await page.getByRole("button", { name: "按站点升序排序" }).click();
  await expect(
    page.getByRole("columnheader", { name: "站点" }),
  ).toHaveAttribute("aria-sort", "ascending");
  await expect(firstRow).toContainText("Alpha");

  await page.getByRole("button", { name: "按站点降序排序" }).click();
  await expect(firstRow).toContainText("Charlie");

  await page.getByRole("button", { name: "按传输量降序排序" }).click();
  await expect(
    page.getByRole("columnheader", { name: "传输量" }),
  ).toHaveAttribute("aria-sort", "descending");
  await expect(firstRow).toContainText("Alpha");

  await page.getByRole("button", { name: "按传输量升序排序" }).click();
  await expect(firstRow).toContainText("Bravo");
});

test("sites list shows only the publish status", async ({ page }) => {
  const tlsRequests: string[] = [];
  page.on("request", (request) => {
    if (new URL(request.url()).pathname.endsWith("/tls-status")) {
      tlsRequests.push(request.url());
    }
  });
  await mockAPI(page);
  await page.goto("/#/sites");

  await expect(
    page.getByRole("columnheader", { name: "发布状态" }),
  ).toBeVisible();
  await expect(page.getByRole("columnheader", { name: "版本" })).toBeVisible();
  const row = page.getByRole("row").filter({ hasText: site.name });
  await expect(row.getByText("V8", { exact: true })).toBeVisible();
  await expect(row.getByText("Cache Version V2", { exact: true })).toHaveCount(
    0,
  );
  await expect(row.getByText("成功", { exact: true })).toHaveCount(1);
  expect(tlsRequests).toEqual([]);

  await row.getByRole("link", { name: `管理 ${site.name}` }).click();
  await expect(page.getByText("缓存版本", { exact: true })).toBeVisible();
  await expect(
    page.getByText("Cache Version V2", { exact: true }),
  ).toBeVisible();
  await expect(page.getByText("Cloudflare 区域", { exact: true })).toHaveCount(
    0,
  );
  await expect(page.getByLabel("Cloudflare Zone ID")).toHaveCount(0);
});

test("certificate workspace shows renewal state and manual actions", async ({
  page,
}, testInfo) => {
  const errors = trackPageErrors(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  await mockAPI(page);
  await page.goto("/#/certificates");

  await expect(
    page.getByRole("heading", { name: "证书", level: 1 }),
  ).toBeVisible();
  const table = page.getByRole("table");
  await expect(table.getByRole("link", { name: "静态资源主站" })).toBeVisible();
  await expect(table.getByText("有效", { exact: true })).toBeVisible();
  await expect(table.getByText("待续期", { exact: true })).toBeVisible();
  await expect(table.getByText("未签发", { exact: true })).toBeVisible();
  await expect(table.getByText("无需证书", { exact: true })).toBeVisible();
  await expect(table.getByText("待发布", { exact: true })).toBeVisible();

  const renewalRequest = page.waitForRequest(
    (request) =>
      new URL(request.url()).pathname === "/api/certificates/site-1/renew" &&
      request.method() === "POST",
  );
  await page.getByRole("button", { name: "手动续期" }).first().click();
  await renewalRequest;
  await expect(page.getByText("证书续期已排队")).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("certificates-desktop.png"),
    fullPage: true,
  });

  await page.setViewportSize({ width: 390, height: 844 });
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "证书", level: 1 }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("certificates-mobile.png"),
    fullPage: true,
  });
  expect(errors).toEqual([]);
});

test("new site discovers its Cloudflare zone from domains", async ({
  page,
}) => {
  await mockAPI(page, {
    "/api/nodes": [
      {
        id: "node-auto",
        name: "edge-auto",
        public_ipv4: "203.0.113.92",
        status: "active",
        capabilities: [],
        applied_version: 1,
        created_at: now.toISOString(),
        updated_at: now.toISOString(),
      },
    ],
  });
  await page.goto("/#/sites/new");

  await expect(page.getByText("Cloudflare 区域", { exact: true })).toHaveCount(
    0,
  );
  await expect(page.getByText("根据域名自动识别", { exact: true })).toHaveCount(
    0,
  );
  await expect(page.getByLabel("Cloudflare Zone ID")).toHaveCount(0);
  await page.getByLabel("站点名称").fill("自动区域站点");
  await page.getByLabel("域名").fill("cdn.auto.example.com");
  await page.getByLabel("源站 URL").fill("https://origin.auto.example.com");
  await page.getByText("edge-auto", { exact: true }).click();

  const createRequest = page.waitForRequest(
    (request) =>
      new URL(request.url()).pathname === "/api/sites" &&
      request.method() === "POST",
  );
  await page.getByRole("button", { name: "创建站点" }).click();
  const request = await createRequest;
  expect(request.postDataJSON()).not.toHaveProperty("zone_id");
  await expect(
    page.getByText("站点已创建，TLS 证书正在自动申请"),
  ).toBeVisible();
});

test("HTTP/3 is opt-in per site and saved explicitly", async ({
  page,
}, testInfo) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  await mockAPI(page, {
    "/api/nodes": [
      {
        id: "node-http3",
        name: "edge-http3",
        public_ipv4: "203.0.113.93",
        status: "active",
        capabilities: ["http3_v1"],
        applied_version: 1,
        created_at: now.toISOString(),
        updated_at: now.toISOString(),
      },
    ],
  });
  await page.goto("/#/sites/new");

  const http3Toggle = page.getByRole("switch", { name: "HTTP/3 / QUIC" });
  await expect(http3Toggle).not.toBeChecked();
  await http3Toggle.click();
  await expect(http3Toggle).toBeChecked();

  await page.getByLabel("站点名称").fill("HTTP3 站点");
  await page.getByLabel("域名").fill("h3.example.com");
  await page.getByLabel("源站 URL").fill("https://origin.example.com");
  await page.getByText("edge-http3", { exact: true }).click();
  await page.screenshot({
    path: testInfo.outputPath("site-http3-opt-in.png"),
    fullPage: true,
  });

  await page.setViewportSize({ width: 390, height: 844 });
  await expect(http3Toggle).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("site-http3-opt-in-mobile.png"),
    fullPage: true,
  });

  const createRequest = page.waitForRequest(
    (request) =>
      new URL(request.url()).pathname === "/api/sites" &&
      request.method() === "POST",
  );
  await page.getByRole("button", { name: "创建站点" }).click();
  const request = await createRequest;
  expect(request.postDataJSON()).toMatchObject({ http3_enabled: true });
});

test("site publish waits while automatic TLS issuance is active", async ({
  page,
}) => {
  let publishRequests = 0;
  page.on("request", (request) => {
    if (
      new URL(request.url()).pathname === "/api/sites/site-1/publish" &&
      request.method() === "POST"
    ) {
      publishRequests += 1;
    }
  });
  await mockAPI(page, {
    "/api/sites": [{ ...site, published: false }],
    "/api/sites/site-1/tls-status": {
      certificate_task: {
        id: "tls-pending",
        kind: "issue_certificate",
        site_id: "site-1",
        status: "applying",
        detail: "waiting for DNS-01 validation",
        created_at: now.toISOString(),
        updated_at: now.toISOString(),
      },
      published_after_certificate: false,
    },
  });
  await page.goto("/#/sites/site-1");

  const operations = page
    .locator('[data-slot="card"]')
    .filter({ hasText: "发布与运维" });
  await operations.getByRole("button", { name: "发布站点" }).click();
  await expect(
    page.getByRole("dialog", { name: "正在申请 TLS 证书" }),
  ).toBeVisible();
  await expect(
    operations.getByRole("button", { name: "签发 TLS" }),
  ).toHaveCount(0);
  expect(publishRequests).toBe(0);
});

test("site operations show current assignments instead of publish task targets", async ({
  page,
}, testInfo) => {
  const previous = new Date(now.getTime() - 60 * 60 * 1000).toISOString();
  const currentSite = {
    ...site,
    name: "API 加速",
    domains: ["api.dustk.com"],
    node_ids: ["gateway", "vmiss-lax"],
    published: true,
    updated_at: now.toISOString(),
  };
  const edgeNodes = [
    ["andnode", "andnode", "203.0.113.10"],
    ["gateway", "gateway", "203.0.113.11"],
    ["geelinx", "geelinx", "203.0.113.12"],
    ["vmiss-lax", "vmiss-lax", "203.0.113.13"],
  ].map(([id, name, publicIPv4]) => ({
    id,
    name,
    public_ipv4: publicIPv4,
    status: "active",
  }));
  await mockAPI(page, {
    "/api/sites": [currentSite],
    "/api/nodes": edgeNodes,
    "/api/sites/site-1/publish-status": {
      task: {
        id: "publish-old",
        kind: "publish_site",
        site_id: "site-1",
        status: "succeeded",
        detail: "configuration applied by 3 active edge node(s)",
        created_at: previous,
        updated_at: previous,
      },
      nodes: [
        {
          node_id: "andnode",
          node_name: "andnode",
          target_version: 7,
          status: "succeeded",
        },
        {
          node_id: "gateway",
          node_name: "gateway",
          target_version: 7,
          status: "succeeded",
        },
        {
          node_id: "geelinx",
          node_name: "geelinx",
          target_version: 7,
          status: "succeeded",
        },
      ],
    },
    "/api/sites/site-1/tls-status": {
      certificate_task: {
        id: "tls-1",
        kind: "issue_certificate",
        site_id: "site-1",
        status: "succeeded",
        detail: "certificate stored; publish the site to deploy it",
        created_at: previous,
        updated_at: previous,
      },
      published_after_certificate: false,
    },
    "/api/sites/site-1/origin-allowlist": {
      site_id: "site-1",
      ipv4_cidrs: ["203.0.113.11/32", "203.0.113.13/32"],
      nodes: [
        {
          node_id: "gateway",
          node_name: "gateway",
          ipv4_cidr: "203.0.113.11/32",
          assignment: "current_and_published",
        },
        {
          node_id: "vmiss-lax",
          node_name: "vmiss-lax",
          ipv4_cidr: "203.0.113.13/32",
          assignment: "current_and_published",
        },
      ],
      note: "源站防火墙或安全组需允许当前配置节点的 IPv4 CIDR。",
    },
  });
  await page.goto("/#/sites/site-1");

  const operations = page
    .locator('[data-slot="card"]')
    .filter({ hasText: "发布与运维" });
  await expect(
    operations.getByText("当前配置已发布到 2 个边缘节点"),
  ).toBeVisible();
  await expect(operations.getByText(/当前承载节点.*2 个/)).toBeVisible();
  await expect(operations.getByText("gateway", { exact: true })).toBeVisible();
  await expect(
    operations.getByText("vmiss-lax", { exact: true }),
  ).toBeVisible();
  await expect(operations.getByText("andnode", { exact: true })).toHaveCount(0);
  await expect(operations.getByText("geelinx", { exact: true })).toHaveCount(0);
  await expect(
    operations.getByText("TLS 证书已保存，请发布站点以部署到边缘节点"),
  ).toBeVisible();
  await expect(operations.getByText(/configuration applied by/)).toHaveCount(0);
  await expect(operations.getByText(/certificate stored/)).toHaveCount(0);
  await page.screenshot({
    path: testInfo.outputPath("site-operations-current-nodes.png"),
    fullPage: true,
  });

  await operations.getByRole("button", { name: "源站白名单" }).click();
  const dialog = page.getByRole("dialog", { name: "源站防火墙白名单" });
  await expect(dialog.getByText("当前配置节点", { exact: true })).toBeVisible();
  await expect(dialog.getByText("gateway", { exact: true })).toBeVisible();
  await expect(dialog.getByText("vmiss-lax", { exact: true })).toBeVisible();
  await expect(dialog.getByText("发布后移除")).toHaveCount(0);
  await expect(dialog.getByText("andnode", { exact: true })).toHaveCount(0);
  await expect(dialog.getByText("geelinx", { exact: true })).toHaveCount(0);
  await page.screenshot({
    path: testInfo.outputPath("origin-allowlist-assignments.png"),
  });
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("origin-allowlist-mobile.png"),
  });
});

test("SMTP test shows progress and keeps timeout feedback visible", async ({
  page,
}) => {
  await mockAPI(page, {
    "/api/settings": {
      branding: {
        name: "simple_cdn",
        subtitle: "控制面板",
        logo_data_url: "",
      },
      cache: { default_size_gb: 1 },
      dns: { default_ttl_seconds: 60 },
      cloudflare: {
        source: "environment",
        configured: true,
        override_configured: false,
        environment_configured: true,
      },
      smtp: {
        enabled: true,
        host: "smtp.example.test",
        port: 465,
        username: "mailer",
        from_address: "cdn@example.test",
        recipients: ["ops@example.test"],
        notification_categories: [
          "availability",
          "monitoring",
          "certificate",
          "backup",
        ],
        security: "tls",
        source: "database",
        override_configured: true,
        password_configured: true,
        environment_configured: false,
      },
      backup: {
        repository: "",
        access_key_id: "",
        region: "us-east-1",
        backup_time: "03:25",
        random_delay_seconds: 1200,
        source: "unconfigured",
        configured: false,
        override_configured: false,
        secret_access_key_configured: false,
        restic_password_configured: false,
        environment_configured: false,
      },
    },
  });
  let releaseFirstRequest: () => void = () => undefined;
  const firstRequestGate = new Promise<void>((resolve) => {
    releaseFirstRequest = resolve;
  });
  let attempts = 0;
  await page.route("**/api/settings/smtp/test", async (route) => {
    attempts += 1;
    if (attempts === 1) {
      await firstRequestGate;
      await route.fulfill({
        status: 504,
        contentType: "application/json",
        body: JSON.stringify({ error: "SMTP connection timed out" }),
      });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ ok: true }),
    });
  });

  await page.goto("/#/settings");
  await page.getByRole("tab", { name: "通知" }).click();
  await page.getByRole("button", { name: "发送测试邮件" }).click();
  const pendingButton = page.getByRole("button", { name: "正在发送" });
  await expect(pendingButton).toBeDisabled();
  await expect(pendingButton).toHaveAttribute("aria-busy", "true");
  await expect(pendingButton.locator(".animate-spin")).toBeVisible();

  releaseFirstRequest();
  const failure = page
    .getByRole("alert")
    .filter({ hasText: "测试邮件发送失败" });
  await expect(failure).toContainText(
    "SMTP 连接超时，请检查服务器、端口、安全连接方式及网络连通性。",
  );
  await page.getByLabel("服务器").fill("smtp-alt.example.test");
  await expect(failure).toBeVisible();

  await page.getByRole("button", { name: "发送测试邮件" }).click();
  await expect(failure).toHaveCount(0);
  await expect(page.getByText("测试邮件已发送")).toBeVisible();
  expect(attempts).toBe(2);
});

test("SMTP alert categories can be saved independently", async ({ page }) => {
  await mockAPI(page);
  let savedCategories: string[] | undefined;
  await page.route("**/api/settings/smtp", async (route) => {
    const input = route.request().postDataJSON() as {
      notification_categories: string[];
    };
    savedCategories = input.notification_categories;
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ ok: true }),
    });
  });

  await page.goto("/#/settings");
  await page.getByRole("tab", { name: "通知" }).click();
  const monitoring = page.getByRole("switch", { name: "TCP 拨测异常" });
  const backup = page.getByRole("switch", { name: "备份任务" });
  await expect(monitoring).toBeChecked();
  await expect(backup).toBeChecked();
  await monitoring.click();
  await backup.click();
  await page.getByRole("button", { name: "保存 SMTP" }).click();

  await expect
    .poll(() => savedCategories)
    .toEqual(["availability", "certificate"]);
});

test("branding settings update the sidebar immediately", async ({
  page,
}, testInfo) => {
  await mockAPI(page);
  await page.goto("/#/settings");

  await expect(page.getByRole("tab", { name: "通用" })).toBeVisible();
  await expect(page.getByRole("tab", { name: "品牌" })).toHaveCount(0);
  await expect(page.getByLabel("品牌标识")).toHaveValue("simple_cdn");
  await expect(page.getByLabel("副标题")).toHaveValue("控制面板");
  await page.getByLabel("品牌标识").fill("DustK Edge");
  await page.getByLabel("副标题").fill("边缘控制台");
  await page.getByLabel("品牌 Logo").setInputFiles({
    name: "dustk-logo.png",
    mimeType: "image/png",
    buffer: Buffer.from(
      "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
      "base64",
    ),
  });
  await page.getByRole("button", { name: "保存通用设置" }).click();

  const sidebar = page.locator('[data-sidebar="sidebar"]');
  await expect(sidebar.getByText("DustK Edge", { exact: true })).toBeVisible();
  await expect(sidebar.getByText("边缘控制台", { exact: true })).toBeVisible();
  await expect(
    sidebar.locator('img[src^="data:image/png;base64,"]'),
  ).toBeVisible();
  await expect(page).toHaveTitle("DustK Edge · 边缘控制台");
  await expect(
    page.locator('link[rel="icon"][data-branding-icon]'),
  ).toHaveAttribute("href", /^data:image\/png;base64,/);

  await page.route("**/api/session", async (route) => {
    await new Promise((resolve) => setTimeout(resolve, 1_000));
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ user: "admin", csrf_token: "e2e-csrf" }),
    });
  });
  await page.reload();
  const bootScreen = page.locator("main");
  await expect(bootScreen.getByText("正在验证登录状态")).toBeVisible();
  await expect(
    bootScreen.getByText("DustK Edge", { exact: true }),
  ).toBeVisible();
  await expect(bootScreen.getByText("simple_cdn", { exact: true })).toHaveCount(
    0,
  );
  await expect(sidebar.getByText("DustK Edge", { exact: true })).toBeVisible();
  await expect(page).toHaveTitle("DustK Edge · 边缘控制台");

  await page.evaluate(() => window.localStorage.clear());
  await page.reload();
  await expect(bootScreen.getByText("正在验证登录状态")).toBeVisible();
  await expect(bootScreen.getByText("simple_cdn", { exact: true })).toHaveCount(
    0,
  );
  await expect(sidebar.getByText("DustK Edge", { exact: true })).toBeVisible();
  await expect(page).toHaveTitle("DustK Edge · 边缘控制台");
  await page.screenshot({
    path: testInfo.outputPath("branding-settings.png"),
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("branding-settings-mobile.png"),
    fullPage: true,
  });
});

test("theme menu supports light dark and system modes", async ({
  page,
}, testInfo) => {
  await page.emulateMedia({ colorScheme: "light" });
  await mockAPI(page);
  await page.goto("/#/overview");

  const sidebar = page.locator('[data-sidebar="sidebar"]');
  await expect(sidebar.getByText("消息中心", { exact: true })).toHaveCount(0);

  await page.getByRole("button", { name: "主题：跟随系统" }).click();
  await page.getByRole("menuitemradio", { name: "深色" }).click();
  await expect(page.locator("html")).toHaveClass(/dark/);
  await expect(page.getByRole("button", { name: "主题：深色" })).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("theme-dark.png"),
    fullPage: true,
  });

  await page.getByRole("button", { name: "主题：深色" }).click();
  await page.getByRole("menuitemradio", { name: "浅色" }).click();
  await expect(page.locator("html")).toHaveClass(/light/);

  await page.getByRole("button", { name: "主题：浅色" }).click();
  await page.getByRole("menuitemradio", { name: "跟随系统" }).click();
  await expect(
    page.getByRole("button", { name: "主题：跟随系统" }),
  ).toBeVisible();

  await page.getByRole("button", { name: "打开消息中心" }).click();
  await expect(page.getByRole("heading", { name: "消息中心" })).toBeVisible();
});

test("language switch localizes workspaces and survives reload", async ({
  page,
}, testInfo) => {
  const errors = trackPageErrors(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  await mockAPI(page);
  await page.goto("/#/overview");

  await page.getByRole("button", { name: "切换语言" }).click();
  await page.getByRole("menuitemradio", { name: "English" }).click();
  await expect(page.locator("html")).toHaveAttribute("lang", "en");
  await expect(
    page.getByRole("heading", { name: "Overview", level: 1 }),
  ).toBeVisible();
  await expect(page.getByRole("link", { name: "Scheduling" })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Theme: System" }),
  ).toBeVisible();

  for (const [path, heading] of [
    ["logs", "Logs"],
    ["monitoring", "Monitoring"],
    ["scheduling", "Scheduling"],
    ["nodes", "Nodes"],
    ["sites", "Sites"],
    ["security", "Security"],
    ["certificates", "Certificates"],
    ["settings", "Settings"],
  ]) {
    await page.goto(`/#/${path}`);
    await expect(
      page.getByRole("heading", { name: heading, level: 1 }),
    ).toBeVisible();
  }

  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("lang", "en");
  await expect(
    page.getByRole("heading", { name: "Settings", level: 1 }),
  ).toBeVisible();

  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/#/scheduling");
  await expect(
    page.getByRole("heading", { name: "Scheduling", level: 1 }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("scheduling-english-mobile.png"),
    fullPage: true,
  });
  expect(errors).toEqual([]);
});

test("mobile sidebar closes after hash navigation without horizontal overflow", async ({
  page,
}, testInfo) => {
  const errors = trackPageErrors(page);
  await page.setViewportSize({ width: 390, height: 844 });
  await mockAPI(page);
  await page.goto("/#/overview");

  await page.getByRole("button", { name: "切换侧边栏" }).click();
  await expect(page.getByText("工作区", { exact: true })).toBeVisible();
  await expect(page.getByLabel("simple_cdn 版本 v0.1.1")).toBeVisible();
  const logLink = page.getByRole("link", { name: "日志" });
  expect(
    await logLink.evaluate((element) => getComputedStyle(element).textAlign),
  ).toBe("left");
  await page.screenshot({
    path: testInfo.outputPath("sidebar-mobile.png"),
    fullPage: true,
  });
  await logLink.click();
  await expect(
    page.getByRole("heading", { name: "日志", level: 1 }),
  ).toBeVisible();
  await expect(page.getByText("工作区", { exact: true })).toBeHidden();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  expect(errors).toEqual([]);

  await page.screenshot({
    path: testInfo.outputPath("logs-mobile.png"),
    fullPage: true,
  });
});

test("security tabs fit on one line without scrollbars", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await mockAPI(page);
  await page.goto("/#/security");

  const tabs = page.locator('[data-slot="tabs-list"]');
  await expect(tabs.getByRole("tab")).toHaveCount(5);
  expect(
    await tabs.evaluate((element) => ({
      horizontalOverflow: element.scrollWidth > element.clientWidth,
      overflowX: getComputedStyle(element).overflowX,
      overflowY: getComputedStyle(element).overflowY,
      rows: new Set(
        Array.from(element.children, (child) =>
          Math.round(child.getBoundingClientRect().top),
        ),
      ).size,
    })),
  ).toEqual({
    horizontalOverflow: false,
    overflowX: "visible",
    overflowY: "visible",
    rows: 1,
  });
});

test("rate limit errors can escalate from 429 to an IP ban", async ({
  page,
}) => {
  const securityOverview = {
    policies: [],
    rate_limit_policies: [],
    bans: [],
    active_ban_count: 0,
    events: [],
    nodes: [],
  };
  await mockAPI(page, {
    "/api/security": securityOverview,
    "/api/security/rate-limit-policies": securityOverview,
  });
  await page.goto("/#/security");
  await page.getByRole("tab", { name: "请求限速" }).click();
  await page.getByRole("button", { name: "新增" }).click();

  await page.getByLabel("名称").fill("API 错误突发");
  await page.getByLabel("每秒请求上限").fill("8");
  await page.getByRole("switch", { name: "连续超限后封禁 IP" }).click();
  await expect(
    page.getByRole("switch", { name: "仅统计指定响应" }),
  ).toBeChecked();
  await expect(
    page.getByRole("switch", { name: "仅统计指定响应" }),
  ).toBeDisabled();
  await expect(page.getByRole("checkbox", { name: "2xx" })).toBeDisabled();
  await expect(page.getByRole("checkbox", { name: "4xx" })).toBeChecked();
  await expect(page.getByRole("checkbox", { name: "5xx" })).toBeChecked();
  await page.getByLabel("连续 429 次数").fill("4");
  const banDuration = page.getByText("封禁时间", { exact: true }).locator("..");
  await banDuration.getByRole("combobox").click();
  await page.getByRole("option", { name: "6 小时" }).click();

  const requestPromise = page.waitForRequest(
    (request) =>
      new URL(request.url()).pathname === "/api/security/rate-limit-policies" &&
      request.method() === "POST",
  );
  await page.getByRole("button", { name: "保存并发布" }).click();
  const request = await requestPromise;
  expect(request.postDataJSON()).toMatchObject({
    name: "API 错误突发",
    requests_per_second: 8,
    response_condition_enabled: true,
    response_status_classes: [4, 5],
    ban_enabled: true,
    ban_after_consecutive_429: 4,
    ban_duration_seconds: 21600,
  });
});

test("log rows truncate long paths, color errors, and open request details", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await mockAPI(page);
  await page.goto("/#/logs");

  const notFoundRow = page.getByRole("row", {
    name: new RegExp(`查看请求 GET ${accessLogs[0].path}`),
  });
  const longPath = notFoundRow.locator("code");
  await expect(longPath).toHaveAttribute("title", accessLogs[0].path);
  expect(
    await longPath.evaluate(
      (element) => element.scrollWidth > element.clientWidth,
    ),
  ).toBe(true);
  const requestCell = notFoundRow.locator("td").nth(1);
  const statusCell = notFoundRow.locator("td").nth(2);
  const [requestBox, statusBox] = await Promise.all([
    requestCell.boundingBox(),
    statusCell.boundingBox(),
  ]);
  expect(requestBox?.x).toBeDefined();
  expect(statusBox?.x).toBeDefined();
  expect((requestBox?.x ?? 0) + (requestBox?.width ?? 0)).toBeLessThanOrEqual(
    (statusBox?.x ?? 0) + 1,
  );
  await expect(notFoundRow.getByText("404", { exact: true })).toHaveClass(
    /bg-warning\/10/,
  );
  const badGatewayRow = page.getByRole("row", {
    name: new RegExp(`查看请求 GET ${accessLogs[1].path}`),
  });
  await expect(badGatewayRow.getByText("502", { exact: true })).toHaveClass(
    /bg-destructive\/10/,
  );

  await notFoundRow.click();
  await expect(
    page.getByRole("heading", { name: "请求详情", level: 1 }),
  ).toBeVisible();
  await expect(page.getByText(accessLogs[0].user_agent)).toBeVisible();
  await expect(page.getByText("请求大小", { exact: true })).toBeVisible();
  await expect(page.getByText("响应大小", { exact: true })).toBeVisible();
  await expect(page.getByText("Range", { exact: true })).toBeVisible();
  await expect(page.getByText("bytes=0-4095", { exact: true })).toBeVisible();
});

test("node machine status updates from the realtime event stream", async ({
  page,
}) => {
  const errors = trackPageErrors(page);
  const node = {
    id: "node-1",
    name: "realtime-edge",
    public_ipv4: "203.0.113.40",
    status: "active",
    nginx_capacity: {
      worker_processes: 0,
      worker_connections: 4096,
      worker_rlimit_nofile: 65536,
    },
    capabilities: ["machine_status_v1", "machine_status_stream_v1"],
    agent_version: "0.1.14",
    target_agent_version: "0.1.14",
    applied_version: 9,
    last_heartbeat_at: now.toISOString(),
    created_at: now.toISOString(),
    updated_at: now.toISOString(),
    upgrade_capable: true,
    upgrade_up_to_date: true,
    can_upgrade: false,
  };
  const machineReport = {
    distribution: "Debian GNU/Linux",
    version: "13.5",
    uptime_seconds: 86_400,
    load_1: 0.1,
    load_5: 0.2,
    load_15: 0.3,
    cpu_usage_percent: 12,
    cpu_logical_cores: 4,
    memory_used_bytes: 2_147_483_648,
    memory_total_bytes: 4_294_967_296,
    disk_used_bytes: 10_737_418_240,
    disk_total_bytes: 53_687_091_200,
    network_interface: "eth0",
    network_rx_bytes_per_second: 2_000,
    network_tx_bytes_per_second: 1_000,
    sample_seconds: 5,
    collected_at: now.toISOString(),
  };
  await mockAPI(page, {
    "/api/nodes/node-1": {
      node,
      machine: { available: true, stale: false, report: machineReport },
      cache: {
        default_size_gb: 1,
        override_size_gb: null,
        effective_size_gb: 1,
      },
      sites: [],
    },
    "/api/nodes/node-1/machine-status/events": {
      available: true,
      stale: false,
      report: {
        ...machineReport,
        cpu_usage_percent: 73,
        network_rx_bytes_per_second: 8_192,
        collected_at: new Date(now.getTime() + 5_000).toISOString(),
      },
    },
    "/api/nodes/node-1/cache-status": {
      available: false,
      unavailable_reason: "暂无缓存访问记录",
      from: series[0].time,
      to: now.toISOString(),
      requests: 0,
      bytes: 0,
      cache_lookups: 0,
      cache_hits: 0,
      cache_misses: 0,
      bypasses: 0,
      uncached: 0,
      hit_rate: 0,
      statuses: [],
      storage: {
        available: false,
        unavailable_reason: "暂无缓存空间数据",
        used_bytes: 0,
        total_bytes: 0,
        stale: false,
      },
    },
    "/api/nodes/node-1/uninstall": {
      node,
      job: null,
      blockers: [],
      can_generate_command: false,
      ready_in_seconds: 0,
    },
  });
  await page.goto("/#/nodes/node-1");

  await expect(
    page.getByRole("heading", { name: "realtime-edge", level: 1 }),
  ).toBeVisible();
  await expect(page.getByText("73.0%", { exact: true })).toBeVisible();
  await expect(page.getByText(/接收\s*8\.0 KiB\/s/)).toBeVisible();
  expect(errors).toEqual([]);
});

test("cache defaults are configurable and overridden by individual nodes", async ({
  page,
}) => {
  const node = {
    id: "node-1",
    name: "cache-edge",
    public_ipv4: "203.0.113.41",
    status: "active",
    nginx_capacity: {
      worker_processes: 0,
      worker_connections: 4096,
      worker_rlimit_nofile: 65536,
    },
    capabilities: [],
    agent_version: "0.1.2",
    target_agent_version: "9.9.9",
    applied_version: 8,
    last_heartbeat_at: now.toISOString(),
    created_at: now.toISOString(),
    updated_at: now.toISOString(),
    upgrade_capable: false,
    upgrade_up_to_date: false,
    can_upgrade: false,
    upgrade_blocker: "升级边缘代理后可使用在线升级",
  };
  await mockAPI(page, {
    "/api/nodes/node-1": {
      node,
      machine: {
        available: false,
        unavailable_reason: "升级边缘代理后可查看机器状态",
        stale: false,
      },
      cache: {
        default_size_gb: 1,
        override_size_gb: null,
        effective_size_gb: 1,
      },
      sites: [],
    },
    "/api/nodes/node-1/cache-status": {
      available: false,
      unavailable_reason: "缓存统计暂不可用",
      from: series[0].time,
      to: now.toISOString(),
      requests: 0,
      bytes: 0,
      cache_lookups: 0,
      cache_hits: 0,
      cache_misses: 0,
      bypasses: 0,
      uncached: 0,
      hit_rate: 0,
      statuses: [],
      storage: {
        available: false,
        unavailable_reason: "升级边缘代理后可查看缓存空间",
        used_bytes: 0,
        total_bytes: 0,
        stale: false,
      },
    },
    "/api/nodes/node-1/uninstall": {
      node,
      job: null,
      blockers: [],
      can_generate_command: false,
      ready_in_seconds: 0,
    },
  });
  await page.goto("/#/settings");
  await page.getByRole("tab", { name: "网络与 DNS" }).click();
  const cacheSize = page.getByLabel("节点默认总上限（GB）");
  await expect(cacheSize).toHaveValue("1");
  await cacheSize.fill("4");
  await page.getByRole("button", { name: "保存缓存配置" }).click();
  await expect(page.getByText("全局缓存上限已保存")).toBeVisible();

  await page.goto("/#/nodes/node-1");
  await expect(page.getByText("v0.1.2", { exact: true })).toBeVisible();
  await expect(page.getByText("v9.9.9", { exact: true })).toBeVisible();
  await expect(page.getByRole("heading", { name: "机器状态" })).toHaveCount(0);
  await expect(page.getByText("全局默认 4 GB")).toBeVisible();
  const override = page.getByLabel("覆写全局缓存配额");
  const nodeCacheSize = page.getByLabel("节点缓存总上限（GB）");
  await expect(override).not.toBeChecked();
  await expect(nodeCacheSize).toBeDisabled();
  await expect(nodeCacheSize).toHaveValue("4");

  await override.click();
  await nodeCacheSize.fill("2");
  await page.getByRole("button", { name: "保存缓存配置" }).click();
  await expect(page.getByText("节点缓存配额已保存")).toBeVisible();
  await expect(page.getByText("当前配置 2 GB")).toBeVisible();
  await page.getByRole("button", { name: "暂停调度" }).click();
  await expect(page.getByText("节点状态已更新，智能路由已关闭")).toBeVisible();
});

test("canceled node uninstall returns the panel to its idle state", async ({
  page,
}) => {
  const node = {
    id: "node-1",
    name: "lightlayer-hk",
    public_ipv4: "203.0.113.42",
    status: "active",
    nginx_capacity: {
      worker_processes: 0,
      worker_connections: 4096,
      worker_rlimit_nofile: 65536,
    },
    capabilities: [],
    applied_version: 47,
    last_heartbeat_at: now.toISOString(),
    created_at: now.toISOString(),
    updated_at: now.toISOString(),
    upgrade_capable: false,
    upgrade_up_to_date: false,
    can_upgrade: false,
    upgrade_blocker: "升级边缘代理后可使用在线升级",
  };
  await mockAPI(page, {
    "/api/nodes/node-1": {
      node,
      machine: {
        available: false,
        unavailable_reason: "升级边缘代理后可查看机器状态",
        stale: false,
      },
      cache: {
        default_size_gb: 1,
        override_size_gb: null,
        effective_size_gb: 1,
      },
      sites: [],
    },
    "/api/nodes/node-1/cache-status": {
      available: false,
      unavailable_reason: "缓存统计暂不可用",
      from: series[0].time,
      to: now.toISOString(),
      requests: 0,
      bytes: 0,
      cache_lookups: 0,
      cache_hits: 0,
      cache_misses: 0,
      bypasses: 0,
      uncached: 0,
      hit_rate: 0,
      statuses: [],
      storage: {
        available: false,
        unavailable_reason: "升级边缘代理后可查看缓存空间",
        used_bytes: 0,
        total_bytes: 0,
        stale: false,
      },
    },
    "/api/nodes/node-1/uninstall": {
      node,
      job: {
        node_id: "node-1",
        status: "canceled",
        previous_status: "draining",
        ready_at: now.toISOString(),
        affected_site_ids: ["site-1"],
        forced: false,
        created_at: now.toISOString(),
        updated_at: now.toISOString(),
      },
      blockers: [
        {
          code: "still_assigned",
          site_id: "site-1",
          site_name: "静态资源主站",
          detail: "remove this node from the site",
        },
      ],
      can_generate_command: false,
      ready_in_seconds: 0,
    },
  });

  await page.goto("/#/nodes/node-1");

  await expect(
    page.getByRole("heading", { name: "lightlayer-hk", level: 1 }),
  ).toBeVisible();
  await expect(page.getByText("已取消", { exact: true })).toHaveCount(0);
  await expect(
    page.getByText("remove this node from the site", { exact: true }),
  ).toHaveCount(0);
  await expect(page.getByRole("button", { name: "准备卸载" })).toBeDisabled();
  await expect(
    page.getByText("暂停调度或撤销授权后才能准备卸载。"),
  ).toBeVisible();
});

test("all primary workspaces and the new-site editor mount without runtime errors", async ({
  page,
}) => {
  const errors = trackPageErrors(page);
  await mockAPI(page);

  for (const [path, heading] of [
    ["security", "安全"],
    ["monitoring", "监测"],
    ["scheduling", "调度"],
    ["nodes", "节点"],
    ["sites", "站点"],
    ["certificates", "证书"],
    ["sites/new", "添加站点"],
    ["settings", "设置"],
  ]) {
    await page.goto(`/#/${path}`);
    await expect(
      page.getByRole("heading", { name: heading, level: 1 }),
    ).toBeVisible();
  }
  await page.getByRole("tab", { name: "备份与恢复" }).click();
  await expect(page.getByText("S3 在线恢复")).toBeVisible();
  await page.reload();
  await expect(page.getByRole("tab", { name: "备份与恢复" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await expect(page.getByText("S3 在线恢复")).toBeVisible();
  expect(errors).toEqual([]);
});

test("bulk node upgrade refreshes the page without opening a result dialog", async ({
  page,
}) => {
  const node = {
    id: "node-1",
    name: "edge-hong-kong",
    public_ipv4: "203.0.113.41",
    status: "active",
    capabilities: ["self_upgrade"],
    agent_version: "0.1.4",
    target_agent_version: "9.9.9",
    applied_version: 8,
    last_heartbeat_at: now.toISOString(),
    created_at: now.toISOString(),
    updated_at: now.toISOString(),
    upgrade_capable: true,
    upgrade_up_to_date: false,
    can_upgrade: true,
  };
  await mockAPI(page, {
    "/api/nodes": [node],
    "/api/nodes/upgrade-all": {
      created: 1,
      already_active: 0,
      up_to_date: 0,
      blocked: 0,
      results: [
        {
          node_id: node.id,
          name: node.name,
          state: "created",
        },
      ],
    },
  });
  await page.goto("/#/nodes");
  await expect(page.getByText(node.name, { exact: true })).toBeVisible();

  const nodesRefresh = page.waitForRequest((request) => {
    const url = new URL(request.url());
    return url.pathname === "/api/nodes" && request.method() === "GET";
  });
  await page.getByRole("button", { name: /全部升级/ }).click();
  await nodesRefresh;

  await expect(page.getByText("已创建 1 个升级任务")).toBeVisible();
  await expect(page.getByRole("dialog", { name: "批量升级结果" })).toHaveCount(
    0,
  );
});

test("node list starts one upgrade without opening node details", async ({
  page,
}, testInfo) => {
  const errors = trackPageErrors(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  const baseNode = {
    public_ipv4: "203.0.113.41",
    status: "active",
    capabilities: ["self_upgrade"],
    agent_version: "0.1.16",
    target_agent_version: "0.1.17",
    applied_version: 8,
    last_heartbeat_at: now.toISOString(),
    created_at: now.toISOString(),
    updated_at: now.toISOString(),
    upgrade_capable: true,
  };
  const upgradeableNode = {
    ...baseNode,
    id: "node-ready",
    name: "edge-ready",
    upgrade_up_to_date: false,
    can_upgrade: true,
  };
  const latestNode = {
    ...baseNode,
    id: "node-latest",
    name: "edge-latest",
    public_ipv4: "203.0.113.42",
    agent_version: "0.1.17",
    upgrade_up_to_date: true,
    can_upgrade: false,
  };
  const applyingNode = {
    ...baseNode,
    id: "node-applying",
    name: "edge-applying",
    public_ipv4: "203.0.113.43",
    upgrade_up_to_date: false,
    can_upgrade: false,
    upgrade_task: {
      id: "upgrade-applying",
      node_id: "node-applying",
      status: "applying",
      target_sha256: "a".repeat(64),
      deadline_at: new Date(now.getTime() + 30 * 60_000).toISOString(),
      created_at: now.toISOString(),
      updated_at: now.toISOString(),
    },
  };
  const queuedTask = {
    id: "upgrade-queued",
    node_id: upgradeableNode.id,
    status: "queued",
    target_sha256: "b".repeat(64),
    deadline_at: new Date(now.getTime() + 30 * 60_000).toISOString(),
    created_at: now.toISOString(),
    updated_at: now.toISOString(),
  };
  await mockAPI(page, {
    "/api/nodes": [upgradeableNode, latestNode, applyingNode],
    "/api/nodes/node-ready/upgrade": {
      ...upgradeableNode,
      can_upgrade: false,
      upgrade_blocker: "节点升级正在进行",
      upgrade_task: queuedTask,
    },
  });
  await page.goto("/#/nodes");

  const upgradeButton = page.getByRole("button", {
    name: "升级节点 edge-ready",
  });
  await expect(upgradeButton).toBeVisible();
  await expect(page.getByText("最新", { exact: true })).toBeVisible();
  await expect(page.getByText("升级中", { exact: true })).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("node-inline-upgrade-ready.png"),
    fullPage: true,
  });

  const upgradeRequest = page.waitForRequest((request) => {
    const url = new URL(request.url());
    return (
      url.pathname === "/api/nodes/node-ready/upgrade" &&
      request.method() === "POST"
    );
  });
  await upgradeButton.click();
  await upgradeRequest;

  await expect(page.getByText("节点 edge-ready 升级已启动")).toBeVisible();
  await expect(
    page.getByRole("button", { name: "edge-ready · 排队中" }),
  ).toBeDisabled();
  await expect(page).toHaveURL(/#\/nodes$/);
  await page.screenshot({
    path: testInfo.outputPath("node-inline-upgrade.png"),
    fullPage: true,
  });

  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  await page.locator('[data-slot="table-container"]').evaluate((container) => {
    container.scrollLeft = container.scrollWidth;
  });
  await page.screenshot({
    path: testInfo.outputPath("node-inline-upgrade-mobile.png"),
    fullPage: true,
  });
  expect(errors).toEqual([]);
});

test("backup restore permanently deletes a confirmed S3 snapshot", async ({
  page,
}, testInfo) => {
  const errors = trackPageErrors(page);
  const snapshotID = "d".repeat(64);
  const snapshot = {
    id: snapshotID,
    short_id: snapshotID.slice(0, 8),
    time: "2026-07-18T01:02:03Z",
    hostname: "control-primary",
    paths: ["/backup/staging/control"],
    tags: ["cdn-control-compose"],
  };
  await page.setViewportSize({ width: 1440, height: 900 });
  await mockAPI(page, { "/api/backups/snapshots": [snapshot] });
  await page.goto("/#/settings");
  await page.getByRole("tab", { name: "备份与恢复" }).click();

  let snapshotRow = page
    .getByRole("row")
    .filter({ hasText: snapshot.short_id });
  await expect(snapshotRow).toBeVisible();
  await expect(
    snapshotRow.getByRole("button", {
      name: `删除快照 ${snapshot.short_id}`,
    }),
  ).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("backup-snapshot-delete-desktop.png"),
    fullPage: true,
  });

  await page.setViewportSize({ width: 390, height: 844 });
  await page.reload();
  await page.getByRole("tab", { name: "备份与恢复" }).click();
  snapshotRow = page.getByRole("row").filter({ hasText: snapshot.short_id });
  await snapshotRow
    .getByRole("button", { name: `删除快照 ${snapshot.short_id}` })
    .click();
  const dialog = page.getByRole("alertdialog", { name: "删除备份快照" });
  await expect(dialog).toContainText("此操作不可撤销");
  await expect(dialog.getByRole("button", { name: "永久删除" })).toBeDisabled();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("backup-snapshot-delete-mobile.png"),
    fullPage: true,
  });

  await dialog.getByRole("textbox").fill(snapshot.short_id);
  await dialog.getByRole("button", { name: "永久删除" }).click();
  await expect(
    page.getByText(`备份快照 ${snapshot.short_id} 已永久删除`),
  ).toBeVisible();
  await expect(
    page.getByRole("row").filter({ hasText: snapshot.short_id }),
  ).toHaveCount(0);
  await expect(page.getByText("没有可用快照")).toBeVisible();
  expect(errors).toEqual([]);
});

test("monitoring workspace shows scoring, probe results, and target controls", async ({
  page,
}, testInfo) => {
  const errors = trackPageErrors(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  await mockAPI(page);
  await page.goto("/#/monitoring");

  await expect(
    page.getByRole("heading", { name: "监测", level: 1 }),
  ).toBeVisible();
  await expect(page.getByText("edge-hong-kong")).toBeVisible();
  await expect(page.getByText("智能暂停")).toBeVisible();
  await expect(page.getByText("96", { exact: true })).toBeVisible();
  await expect(page.getByRole("tab", { name: "智能路由" })).toHaveCount(0);
  await page.screenshot({
    path: testInfo.outputPath("monitoring-desktop.png"),
    fullPage: true,
  });

  await page.setViewportSize({ width: 390, height: 844 });
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "监测", level: 1 }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("monitoring-mobile.png"),
    fullPage: true,
  });

  await page.setViewportSize({ width: 1440, height: 900 });

  await page.getByRole("tab", { name: "拨测明细" }).click();
  await expect(page.getByText("connect: connection timed out")).toBeVisible();
  await expect(page.getByText("3 / 3").first()).toBeVisible();

  await page.getByRole("tab", { name: "目标配置" }).click();
  await expect(page.getByText("probe-a.example.com:443")).toBeVisible();
  await page.getByRole("button", { name: "添加目标" }).click();
  await page.getByLabel("名称").fill("新探针");
  await page
    .getByLabel("IP:端口 或 域名:端口")
    .fill("probe-new.example.com:9443");
  await page.getByRole("button", { name: "添加", exact: true }).click();
  await expect(page.getByText("拨测目标已添加")).toBeVisible();
  await page.getByRole("button", { name: "重命名 主 API" }).click();
  const renameDialog = page.getByRole("dialog");
  await renameDialog.getByLabel("名称").fill("核心 API");
  await renameDialog.getByRole("button", { name: "保存" }).click();
  await expect(page.getByText("拨测目标名称已更新")).toBeVisible();
  await page.reload();
  await expect(page.getByRole("tab", { name: "目标配置" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await expect(page.getByText("probe-a.example.com:443")).toBeVisible();
  expect(errors).toEqual([]);
});

test("scheduling workspace edits per-node score and schedule gates", async ({
  page,
}, testInfo) => {
  const errors = trackPageErrors(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  await mockAPI(page);
  await page.goto("/#/scheduling");

  await expect(
    page.getByRole("heading", { name: "调度", level: 1 }),
  ).toBeVisible();
  await expect(page.getByText("评分、时间", { exact: true })).toBeVisible();
  await expect(page.getByText("窗口内", { exact: true })).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("smart-routing-desktop.png"),
    fullPage: true,
  });

  await page
    .getByRole("button", { name: "编辑 edge-hong-kong 智能路由" })
    .click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByText("Asia/Shanghai")).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("smart-routing-dialog.png"),
    fullPage: true,
  });
  await dialog.getByLabel("暂停分数").fill("75");
  const requestPromise = page.waitForRequest(
    (request) =>
      request.url().endsWith("/api/monitoring/nodes/node-1/smart-routing") &&
      request.method() === "PUT",
  );
  await dialog.getByRole("button", { name: "保存" }).click();
  const request = await requestPromise;
  expect(request.postDataJSON()).toMatchObject({
    enabled: true,
    score: {
      enabled: true,
      pause_below_score: 75,
      pause_after_rounds: 4,
      resume_at_score: 85,
      resume_after_rounds: 3,
    },
    schedule: {
      enabled: true,
      windows: [
        {
          weekdays: [1, 2, 3, 4, 5, 6, 7],
          start: "08:00",
          end: "23:00",
        },
      ],
    },
  });
  await expect(page.getByText("智能路由设置已更新")).toBeVisible();

  await page.setViewportSize({ width: 390, height: 844 });
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "调度", level: 1 }),
  ).toBeVisible();
  await expect(page.getByText("edge-hong-kong")).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("smart-routing-mobile.png"),
    fullPage: true,
  });
  expect(errors).toEqual([]);
});

test("monitoring node history overlays named targets and switches range", async ({
  page,
}, testInfo) => {
  const errors = trackPageErrors(page);
  await page.setViewportSize({ width: 1440, height: 900 });
  await mockAPI(page);
  await page.goto("/#/monitoring");
  await page
    .getByRole("link", { name: "查看 edge-hong-kong 拨测历史" })
    .click();

  await expect(
    page.getByRole("heading", { name: "edge-hong-kong", level: 1 }),
  ).toBeVisible();
  await expect(page.getByText("主 API", { exact: true })).toBeVisible();
  await expect(page.getByText("备用入口", { exact: true })).toBeVisible();
  const chart = page.getByTestId("monitoring-history-chart");
  await expect(chart).toHaveAttribute("data-series-count", "2");
  await expect(chart.locator(".recharts-line-curve")).toHaveCount(2);

  const sevenDayResponse = page.waitForResponse((response) => {
    const url = new URL(response.url());
    return (
      url.pathname === "/api/monitoring/nodes/node-1/history" &&
      url.searchParams.get("range") === "7d"
    );
  });
  const sevenDayTab = page.getByRole("tab", { name: "7 天" });
  await sevenDayTab.click();
  await sevenDayResponse;
  await expect(sevenDayTab).toHaveAttribute("aria-selected", "true");
  await page.reload();
  await expect(page.getByRole("tab", { name: "7 天" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await page.screenshot({
    path: testInfo.outputPath("monitoring-history-desktop.png"),
    fullPage: true,
  });

  await page.setViewportSize({ width: 390, height: 844 });
  await expect(chart).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("monitoring-history-mobile.png"),
    fullPage: true,
  });
  expect(errors).toEqual([]);
});

test("login screen renders without an authenticated session", async ({
  page,
}, testInfo) => {
  const errors = trackPageErrors(page);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.route("**/api/session", (route) =>
    route.fulfill({
      status: 401,
      contentType: "application/json",
      body: JSON.stringify({ error: "authentication required" }),
    }),
  );
  await page.route("**/api/setup/status", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ initialized: true }),
    }),
  );
  await page.goto("/");

  await expect(page.getByRole("heading", { name: "登录控制面" })).toBeVisible();
  await expect(page.getByLabel("管理员密码")).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  expect(errors).toEqual([]);
  await page.screenshot({
    path: testInfo.outputPath("login-mobile.png"),
    fullPage: true,
  });
  await page.getByRole("button", { name: "切换语言" }).click();
  await page.getByRole("menuitemradio", { name: "English" }).click();
  await expect(
    page.getByRole("heading", { name: "Sign in to the control plane" }),
  ).toBeVisible();
  await expect(page.getByLabel("Administrator password")).toBeVisible();
});

test("initial setup requires the local one-time token", async ({
  page,
}, testInfo) => {
  const errors = trackPageErrors(page);
  await page.route("**/api/session", (route) =>
    route.fulfill({
      status: 401,
      contentType: "application/json",
      body: JSON.stringify({ error: "authentication required" }),
    }),
  );
  await page.route("**/api/setup/status", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ initialized: false }),
    }),
  );
  await page.route("**/api/branding", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        name: "simple_cdn",
        subtitle: "控制面板",
        logo_data_url: "",
      }),
    }),
  );

  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/");
  await expect(
    page.getByRole("heading", { name: "初始化控制面" }),
  ).toBeVisible();
  await expect(page.getByLabel("一次性初始化令牌")).toBeVisible();
  await expect(page.getByLabel("管理员密码")).toBeVisible();
  await expect(page.getByLabel("确认密码")).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("initial-setup-desktop.png"),
    fullPage: true,
  });

  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth + 1,
    ),
  ).toBe(true);
  await page.screenshot({
    path: testInfo.outputPath("initial-setup-mobile.png"),
    fullPage: true,
  });
  await page.getByRole("button", { name: "切换语言" }).click();
  await page.getByRole("menuitemradio", { name: "English" }).click();
  await expect(page.getByLabel("One-time initialization token")).toBeVisible();
  expect(errors).toEqual([]);
});

function trackPageErrors(page: Page) {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  return errors;
}
