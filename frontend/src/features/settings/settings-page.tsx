import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Cloud,
  HardDrive,
  ImagePlus,
  LoaderCircle,
  Mail,
  Palette,
  RefreshCw,
  RotateCcw,
  Save,
  Send,
  ShieldCheck,
  Trash2,
  TriangleAlert,
} from "lucide-react";
import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { toast } from "sonner";

import { BrandMark } from "@/components/brand-mark";
import { BackupRestore } from "@/features/settings/backup-restore";
import {
  PageBody,
  PageError,
  PageHeader,
  PageLoading,
} from "@/components/page";
import { StatusBadge } from "@/components/status-badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { cacheBranding, DEFAULT_BRANDING } from "@/hooks/use-branding";
import { api, ApiError, errorMessage } from "@/lib/api";
import type { Settings } from "@/lib/types";

export function SettingsPage() {
  const query = useQuery({
    queryKey: ["settings"],
    queryFn: () => api<Settings>("/api/settings"),
  });
  return (
    <>
      <PageHeader
        title="设置"
        description="通用、运行参数与外部集成"
        actions={
          <Button
            variant="outline"
            size="icon-sm"
            aria-label="刷新设置"
            onClick={() => void query.refetch()}
          >
            <RefreshCw
              className={query.isFetching ? "animate-spin" : undefined}
            />
          </Button>
        }
      />
      <PageBody>
        {query.isLoading ? <PageLoading /> : null}
        {query.error ? <PageError error={query.error} /> : null}
        {query.data ? (
          <Tabs defaultValue="common" className="space-y-5">
            <TabsList>
              <TabsTrigger value="common">通用</TabsTrigger>
              <TabsTrigger value="network">网络与 DNS</TabsTrigger>
              <TabsTrigger value="notifications">通知</TabsTrigger>
              <TabsTrigger value="backup">备份与恢复</TabsTrigger>
            </TabsList>
            <TabsContent value="common">
              <BrandingForm settings={query.data} />
            </TabsContent>
            <TabsContent value="network" className="grid gap-4 lg:grid-cols-2">
              <CacheForm
                key={`cache-${query.data.cache.default_size_gb}`}
                settings={query.data}
              />
              <DNSForm
                key={`dns-${query.data.dns.default_ttl_seconds}`}
                settings={query.data}
              />
              <CloudflareForm
                key={`cf-${query.data.cloudflare.source}-${query.data.cloudflare.configured}`}
                settings={query.data}
              />
            </TabsContent>
            <TabsContent value="notifications">
              <SMTPForm
                key={`smtp-${query.data.smtp.source}-${query.data.smtp.host}-${query.data.smtp.port}-${query.data.smtp.notification_categories.join(",")}`}
                settings={query.data}
              />
            </TabsContent>
            <TabsContent value="backup" className="space-y-5">
              <BackupForm
                key={`backup-${query.data.backup.source}-${query.data.backup.repository}`}
                settings={query.data}
              />
              <BackupRestore />
            </TabsContent>
          </Tabs>
        ) : null}
      </PageBody>
    </>
  );
}

function BrandingForm({ settings }: { settings: Settings }) {
  const queryClient = useQueryClient();
  const [name, setName] = useState(settings.branding.name);
  const [subtitle, setSubtitle] = useState(settings.branding.subtitle);
  const [logoDataURL, setLogoDataURL] = useState(
    settings.branding.logo_data_url,
  );
  useEffect(() => {
    setName(settings.branding.name);
    setSubtitle(settings.branding.subtitle);
    setLogoDataURL(settings.branding.logo_data_url);
  }, [settings.branding]);
  const mutation = useMutation({
    mutationFn: () =>
      api<Settings["branding"]>("/api/settings/branding", {
        method: "PUT",
        body: JSON.stringify({ name, subtitle, logo_data_url: logoDataURL }),
      }),
    onSuccess: (branding) => {
      cacheBranding(branding);
      queryClient.setQueryData<Settings>(["settings"], (current) =>
        current ? { ...current, branding } : current,
      );
      toast.success("通用设置已保存");
    },
    onError: (error) => toast.error(errorMessage(error)),
  });

  return (
    <FormCard
      title="控制台标识"
      description="应用名称与品牌图片"
      icon={<Palette />}
      source="控制台设置"
    >
      <form
        className="grid gap-5"
        onSubmit={(event) => {
          event.preventDefault();
          mutation.mutate();
        }}
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="品牌标识" id="brand-name">
            <Input
              id="brand-name"
              required
              maxLength={48}
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          </Field>
          <Field label="副标题" id="brand-subtitle">
            <Input
              id="brand-subtitle"
              maxLength={80}
              value={subtitle}
              onChange={(event) => setSubtitle(event.target.value)}
            />
          </Field>
        </div>
        <div className="grid gap-2">
          <Label htmlFor="brand-logo">品牌 Logo</Label>
          <input
            id="brand-logo"
            type="file"
            accept="image/png,image/jpeg"
            className="sr-only"
            aria-label="品牌 Logo"
            onChange={(event) => {
              const input = event.currentTarget;
              const file = input.files?.[0];
              input.value = "";
              if (file) {
                void readBrandLogo(file).then(setLogoDataURL, (error) =>
                  toast.error(
                    error instanceof Error ? error.message : "无法读取 Logo",
                  ),
                );
              }
            }}
          />
          <div className="flex flex-wrap items-center gap-2">
            <Button asChild variant="outline">
              <label htmlFor="brand-logo" className="cursor-pointer">
                <ImagePlus />
                选择图片
              </label>
            </Button>
            {logoDataURL ? (
              <Button
                type="button"
                variant="outline"
                onClick={() => setLogoDataURL("")}
              >
                <Trash2 />
                移除 Logo
              </Button>
            ) : null}
            <span className="text-xs text-muted-foreground">
              PNG 或 JPEG，最大 128 KiB
            </span>
          </div>
        </div>
        <div className="grid gap-2">
          <Label>预览</Label>
          <div className="flex min-h-16 w-full max-w-sm items-center gap-3 rounded-lg border bg-sidebar px-3 py-2 text-sidebar-foreground">
            <BrandMark logoDataURL={logoDataURL} className="size-8" />
            <span className="grid min-w-0 text-left leading-tight">
              <span className="truncate font-semibold">
                {name.trim() || DEFAULT_BRANDING.name}
              </span>
              {subtitle.trim() ? (
                <span className="truncate text-xs text-muted-foreground">
                  {subtitle.trim()}
                </span>
              ) : null}
            </span>
          </div>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button type="submit" disabled={mutation.isPending || !name.trim()}>
            {mutation.isPending ? (
              <LoaderCircle className="animate-spin" />
            ) : (
              <Save />
            )}
            保存通用设置
          </Button>
          <Button
            type="button"
            variant="outline"
            disabled={mutation.isPending}
            onClick={() => {
              setName(DEFAULT_BRANDING.name);
              setSubtitle(DEFAULT_BRANDING.subtitle);
              setLogoDataURL(DEFAULT_BRANDING.logo_data_url);
            }}
          >
            <RotateCcw />
            恢复默认值
          </Button>
        </div>
      </form>
    </FormCard>
  );
}

const MAX_BRAND_LOGO_BYTES = 128 << 10;

function readBrandLogo(file: File): Promise<string> {
  if (file.type !== "image/png" && file.type !== "image/jpeg") {
    return Promise.reject(new Error("Logo 仅支持 PNG 或 JPEG 图片"));
  }
  if (file.size > MAX_BRAND_LOGO_BYTES) {
    return Promise.reject(new Error("Logo 图片不能超过 128 KiB"));
  }
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () =>
      typeof reader.result === "string"
        ? resolve(reader.result)
        : reject(new Error("无法读取 Logo"));
    reader.onerror = () => reject(new Error("无法读取 Logo"));
    reader.readAsDataURL(file);
  });
}

function DNSForm({ settings }: { settings: Settings }) {
  const queryClient = useQueryClient();
  const [ttl, setTTL] = useState(settings.dns.default_ttl_seconds);
  const mutation = useMutation({
    mutationFn: () =>
      api("/api/settings/dns", {
        method: "PUT",
        body: JSON.stringify({ default_ttl_seconds: ttl }),
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["settings"] });
      toast.success("DNS 默认 TTL 已保存");
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  return (
    <FormCard
      title="DNS"
      description="全局站点默认参数"
      icon={<Cloud />}
      source="控制面设置"
    >
      <form
        className="grid gap-4"
        onSubmit={(event) => {
          event.preventDefault();
          mutation.mutate();
        }}
      >
        <Field label="全局默认 TTL（秒）" id="dns-ttl">
          <Input
            id="dns-ttl"
            type="number"
            min={60}
            max={300}
            required
            value={ttl}
            onChange={(event) => setTTL(Number(event.target.value))}
          />
        </Field>
        <p className="text-xs text-muted-foreground">
          有效范围 60–300 秒，站点可单独覆盖。
        </p>
        <div>
          <Button type="submit" disabled={mutation.isPending}>
            {mutation.isPending ? (
              <LoaderCircle className="animate-spin" />
            ) : (
              <Save />
            )}
            保存 DNS
          </Button>
        </div>
      </form>
    </FormCard>
  );
}

function CacheForm({ settings }: { settings: Settings }) {
  const queryClient = useQueryClient();
  const [size, setSize] = useState(settings.cache.default_size_gb);
  const mutation = useMutation({
    mutationFn: () =>
      api("/api/settings/cache", {
        method: "PUT",
        body: JSON.stringify({ default_size_gb: size }),
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["settings"] });
      toast.success("全局缓存上限已保存");
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  return (
    <FormCard
      title="缓存"
      description="节点默认磁盘配额"
      icon={<HardDrive />}
      source="控制面设置"
    >
      <form
        className="grid gap-4"
        onSubmit={(event) => {
          event.preventDefault();
          mutation.mutate();
        }}
      >
        <Field label="节点默认总上限（GB）" id="cache-size">
          <Input
            id="cache-size"
            type="number"
            min={1}
            max={1024}
            required
            value={size}
            onChange={(event) => setSize(Number(event.target.value))}
          />
        </Field>
        <p className="text-xs text-muted-foreground">
          未单独覆写的节点在下次发布时使用该总上限。
        </p>
        <div>
          <Button type="submit" disabled={mutation.isPending}>
            {mutation.isPending ? (
              <LoaderCircle className="animate-spin" />
            ) : (
              <Save />
            )}
            保存缓存配置
          </Button>
        </div>
      </form>
    </FormCard>
  );
}

function CloudflareForm({ settings }: { settings: Settings }) {
  const queryClient = useQueryClient();
  const [token, setToken] = useState("");
  const done = (message: string) => {
    void queryClient.invalidateQueries({ queryKey: ["settings"] });
    setToken("");
    toast.success(message);
  };
  const save = useMutation({
    mutationFn: () =>
      api("/api/settings/cloudflare", {
        method: "PUT",
        body: JSON.stringify({ token }),
      }),
    onSuccess: () => done("Cloudflare Token 已验证并保存"),
    onError: (error) => toast.error(errorMessage(error)),
  });
  const test = useMutation({
    mutationFn: () =>
      api("/api/settings/cloudflare/test", {
        method: "POST",
        body: JSON.stringify({ token }),
      }),
    onSuccess: () => toast.success("Cloudflare 配置验证成功"),
    onError: (error) => toast.error(errorMessage(error)),
  });
  const reset = useMutation({
    mutationFn: () => api("/api/settings/cloudflare", { method: "DELETE" }),
    onSuccess: () => done("已恢复 Cloudflare 环境变量配置"),
    onError: (error) => toast.error(errorMessage(error)),
  });
  const busy = save.isPending || test.isPending || reset.isPending;
  return (
    <FormCard
      title="Cloudflare"
      description="托管 DNS API 凭证"
      icon={<ShieldCheck />}
      source={sourceLabel(settings.cloudflare.source)}
    >
      <form
        className="grid gap-4"
        onSubmit={(event) => {
          event.preventDefault();
          save.mutate();
        }}
      >
        <Field label="API Token" id="cloudflare-token">
          <Input
            id="cloudflare-token"
            type="password"
            required={!settings.cloudflare.configured}
            value={token}
            onChange={(event) => setToken(event.target.value)}
            placeholder={
              settings.cloudflare.configured
                ? "已配置，输入新 Token 以替换"
                : "输入 API Token"
            }
            autoComplete="new-password"
          />
        </Field>
        <div className="flex flex-wrap gap-2">
          <Button type="submit" disabled={busy || !token}>
            <Save />
            验证并保存
          </Button>
          <Button
            type="button"
            variant="outline"
            disabled={busy || (!settings.cloudflare.configured && !token)}
            onClick={() => test.mutate()}
          >
            <ShieldCheck />
            验证配置
          </Button>
          <Button
            type="button"
            variant="outline"
            disabled={busy || !settings.cloudflare.override_configured}
            onClick={() => reset.mutate()}
          >
            <RotateCcw />
            恢复环境变量
          </Button>
        </div>
      </form>
    </FormCard>
  );
}

function SMTPForm({ settings }: { settings: Settings }) {
  const queryClient = useQueryClient();
  const initial = settings.smtp;
  const [enabled, setEnabled] = useState(initial.enabled);
  const [host, setHost] = useState(initial.host);
  const [port, setPort] = useState(initial.port || 587);
  const [security, setSecurity] = useState(initial.security || "starttls");
  const [username, setUsername] = useState(initial.username);
  const [password, setPassword] = useState("");
  const [from, setFrom] = useState(initial.from_address);
  const [recipients, setRecipients] = useState(initial.recipients.join(", "));
  const [notificationCategories, setNotificationCategories] = useState(
    initial.notification_categories,
  );
  const [testError, setTestError] = useState("");
  const payload = () => ({
    enabled,
    host,
    port,
    security,
    username,
    from_address: from,
    recipients: split(recipients),
    notification_categories: notificationCategories,
    ...(password ? { password } : {}),
  });
  const done = (message: string) => {
    void queryClient.invalidateQueries({ queryKey: ["settings"] });
    setPassword("");
    toast.success(message);
  };
  const save = useMutation({
    mutationFn: () =>
      api("/api/settings/smtp", {
        method: "PUT",
        body: JSON.stringify(payload()),
      }),
    onSuccess: () => done("SMTP 配置已保存"),
    onError: (error) => toast.error(errorMessage(error)),
  });
  const test = useMutation({
    mutationFn: () =>
      api("/api/settings/smtp/test", {
        method: "POST",
        body: JSON.stringify(payload()),
      }),
    onMutate: () => setTestError(""),
    onSuccess: () => {
      setTestError("");
      toast.success("测试邮件已发送");
    },
    onError: (error) => {
      const message = smtpTestErrorMessage(error);
      setTestError(message);
      toast.error(message);
    },
  });
  const reset = useMutation({
    mutationFn: () => api("/api/settings/smtp", { method: "DELETE" }),
    onSuccess: () => done("已恢复 SMTP 环境变量配置"),
    onError: (error) => toast.error(errorMessage(error)),
  });
  const busy = save.isPending || test.isPending || reset.isPending;
  function submit(event: FormEvent) {
    event.preventDefault();
    save.mutate();
  }
  function setNotificationCategory(category: string, checked: boolean) {
    setNotificationCategories((current) =>
      checked
        ? [...new Set([...current, category])]
        : current.filter((candidate) => candidate !== category),
    );
  }
  return (
    <FormCard
      title="SMTP"
      description="控制面邮件通知通道"
      icon={<Mail />}
      source={sourceLabel(initial.source)}
    >
      <form className="grid gap-5" onSubmit={submit}>
        <div className="flex items-center justify-between rounded-lg border px-3 py-3">
          <div>
            <Label htmlFor="smtp-enabled">启用发信</Label>
            <p className="text-xs text-muted-foreground">
              任务失败和运维事件通知
            </p>
          </div>
          <Switch
            id="smtp-enabled"
            checked={enabled}
            onCheckedChange={setEnabled}
          />
        </div>
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          <Field label="服务器" id="smtp-host">
            <Input
              id="smtp-host"
              required={enabled}
              value={host}
              onChange={(event) => setHost(event.target.value)}
            />
          </Field>
          <Field label="端口" id="smtp-port">
            <Input
              id="smtp-port"
              type="number"
              min={1}
              max={65535}
              required={enabled}
              value={port}
              onChange={(event) => setPort(Number(event.target.value))}
            />
          </Field>
          <Field label="安全连接" id="smtp-security">
            <Select value={security} onValueChange={setSecurity}>
              <SelectTrigger id="smtp-security" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="starttls">STARTTLS</SelectItem>
                <SelectItem value="tls">隐式 TLS</SelectItem>
              </SelectContent>
            </Select>
          </Field>
          <Field label="用户名" id="smtp-user">
            <Input
              id="smtp-user"
              value={username}
              onChange={(event) => setUsername(event.target.value)}
              autoComplete="username"
            />
          </Field>
          <Field label="密码" id="smtp-password">
            <Input
              id="smtp-password"
              type="password"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              placeholder={
                initial.password_configured ? "已保存，留空保持不变" : ""
              }
              autoComplete="new-password"
            />
          </Field>
          <Field label="发件人" id="smtp-from">
            <Input
              id="smtp-from"
              type="email"
              required={enabled}
              value={from}
              onChange={(event) => setFrom(event.target.value)}
            />
          </Field>
          <div className="grid gap-2 sm:col-span-2 lg:col-span-3">
            <Label htmlFor="smtp-recipients">收件人</Label>
            <Input
              id="smtp-recipients"
              required={enabled}
              value={recipients}
              onChange={(event) => setRecipients(event.target.value)}
              placeholder="ops@example.com, admin@example.com"
            />
          </div>
        </div>
        <div className="grid gap-3">
          <div>
            <Label>告警分类</Label>
            <p className="text-xs text-muted-foreground">
              每类通知可独立启用，测试邮件不受分类开关影响
            </p>
          </div>
          <div className="grid overflow-hidden rounded-lg border sm:grid-cols-2">
            {smtpNotificationCategories.map((category, index) => (
              <div
                key={category.value}
                className={`flex min-h-20 items-center justify-between gap-4 px-3 py-3 ${
                  index < 3 ? "border-b" : ""
                } ${index % 2 === 0 ? "sm:border-r" : ""} ${
                  index === 2 ? "sm:border-b-0" : ""
                }`}
              >
                <div className="min-w-0">
                  <Label htmlFor={`smtp-category-${category.value}`}>
                    {category.label}
                  </Label>
                  <p className="mt-1 text-xs leading-5 text-muted-foreground">
                    {category.description}
                  </p>
                </div>
                <Switch
                  id={`smtp-category-${category.value}`}
                  checked={notificationCategories.includes(category.value)}
                  onCheckedChange={(checked) =>
                    setNotificationCategory(category.value, checked)
                  }
                />
              </div>
            ))}
          </div>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button type="submit" disabled={busy}>
            <Save />
            保存 SMTP
          </Button>
          <Button
            type="button"
            variant="outline"
            className="min-w-32"
            disabled={busy || !enabled}
            aria-busy={test.isPending}
            onClick={() => test.mutate()}
          >
            {test.isPending ? (
              <LoaderCircle className="animate-spin" />
            ) : (
              <Send />
            )}
            {test.isPending ? "正在发送" : "发送测试邮件"}
          </Button>
          <Button
            type="button"
            variant="outline"
            disabled={busy || !initial.override_configured}
            onClick={() => reset.mutate()}
          >
            <RotateCcw />
            恢复环境变量
          </Button>
        </div>
        {testError ? (
          <Alert variant="destructive">
            <TriangleAlert />
            <AlertTitle>测试邮件发送失败</AlertTitle>
            <AlertDescription>{testError}</AlertDescription>
          </Alert>
        ) : null}
      </form>
    </FormCard>
  );
}

const smtpNotificationCategories = [
  {
    value: "availability",
    label: "节点与站点可用性",
    description: "健康检查失败、DNS 池调整和无健康节点",
  },
  {
    value: "monitoring",
    label: "TCP 拨测异常",
    description: "拨测异常自动暂停及恢复流量",
  },
  {
    value: "certificate",
    label: "证书续期",
    description: "证书自动续期任务失败",
  },
  {
    value: "backup",
    label: "备份任务",
    description: "自动备份任务失败",
  },
] as const;

function smtpTestErrorMessage(error: unknown) {
  if (error instanceof ApiError && error.status === 504) {
    return "SMTP 连接超时，请检查服务器、端口、安全连接方式及网络连通性。";
  }
  return errorMessage(error);
}

function BackupForm({ settings }: { settings: Settings }) {
  const queryClient = useQueryClient();
  const initial = settings.backup;
  const [repository, setRepository] = useState(initial.repository);
  const [accessKey, setAccessKey] = useState(initial.access_key_id);
  const [secretKey, setSecretKey] = useState("");
  const [region, setRegion] = useState(initial.region || "us-east-1");
  const [resticPassword, setResticPassword] = useState("");
  const [backupTime, setBackupTime] = useState(initial.backup_time || "03:25");
  const [delay, setDelay] = useState(initial.random_delay_seconds ?? 1200);
  const payload = () => ({
    repository,
    access_key_id: accessKey,
    region,
    backup_time: backupTime,
    random_delay_seconds: delay,
    ...(secretKey ? { secret_access_key: secretKey } : {}),
    ...(resticPassword ? { restic_password: resticPassword } : {}),
  });
  const done = (message: string) => {
    void queryClient.invalidateQueries({ queryKey: ["settings"] });
    setSecretKey("");
    setResticPassword("");
    toast.success(message);
  };
  const save = useMutation({
    mutationFn: () =>
      api("/api/settings/backup", {
        method: "PUT",
        body: JSON.stringify(payload()),
      }),
    onSuccess: () => done("S3 备份配置已保存"),
    onError: (error) => toast.error(errorMessage(error)),
  });
  const test = useMutation({
    mutationFn: () =>
      api("/api/settings/backup/test", {
        method: "POST",
        body: JSON.stringify(payload()),
      }),
    onSuccess: () => toast.success("备份仓库验证成功"),
    onError: (error) => toast.error(errorMessage(error)),
  });
  const reset = useMutation({
    mutationFn: () => api("/api/settings/backup", { method: "DELETE" }),
    onSuccess: () => done("已恢复备份环境变量配置"),
    onError: (error) => toast.error(errorMessage(error)),
  });
  const busy = save.isPending || test.isPending || reset.isPending;
  return (
    <FormCard
      title="S3 备份"
      description="Restic 仓库与每日备份计划"
      icon={<Cloud />}
      source={sourceLabel(initial.source)}
    >
      <form
        className="grid gap-5"
        onSubmit={(event) => {
          event.preventDefault();
          save.mutate();
        }}
      >
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          <div className="grid gap-2 sm:col-span-2 lg:col-span-3">
            <Label htmlFor="backup-repository">仓库地址</Label>
            <Input
              id="backup-repository"
              required
              maxLength={2048}
              value={repository}
              onChange={(event) => setRepository(event.target.value)}
              placeholder="s3:https://account.r2.cloudflarestorage.com/bucket"
            />
          </div>
          <Field label="Access Key ID" id="backup-key">
            <Input
              id="backup-key"
              required
              value={accessKey}
              onChange={(event) => setAccessKey(event.target.value)}
            />
          </Field>
          <Field label="Secret Access Key" id="backup-secret">
            <Input
              id="backup-secret"
              type="password"
              required={!initial.secret_access_key_configured}
              value={secretKey}
              onChange={(event) => setSecretKey(event.target.value)}
              placeholder={
                initial.secret_access_key_configured
                  ? "已保存，留空保持不变"
                  : ""
              }
            />
          </Field>
          <Field label="Region" id="backup-region">
            <Input
              id="backup-region"
              required
              value={region}
              onChange={(event) => setRegion(event.target.value)}
            />
          </Field>
          <Field label="Restic 仓库密码" id="restic-password">
            <Input
              id="restic-password"
              type="password"
              required={!initial.restic_password_configured}
              value={resticPassword}
              onChange={(event) => setResticPassword(event.target.value)}
              placeholder={
                initial.restic_password_configured ? "已保存，留空保持不变" : ""
              }
            />
          </Field>
          <Field label="每日执行时间（Asia/Shanghai）" id="backup-time">
            <Input
              id="backup-time"
              type="time"
              required
              value={backupTime}
              onChange={(event) => setBackupTime(event.target.value)}
            />
          </Field>
          <Field label="随机延迟" id="backup-delay">
            <Select
              value={String(delay)}
              onValueChange={(value) => setDelay(Number(value))}
            >
              <SelectTrigger id="backup-delay" className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="0">不延迟</SelectItem>
                <SelectItem value="300">最多 5 分钟</SelectItem>
                <SelectItem value="600">最多 10 分钟</SelectItem>
                <SelectItem value="1200">最多 20 分钟</SelectItem>
                <SelectItem value="1800">最多 30 分钟</SelectItem>
                <SelectItem value="3600">最多 60 分钟</SelectItem>
              </SelectContent>
            </Select>
          </Field>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button type="submit" disabled={busy}>
            <Save />
            保存 S3 备份
          </Button>
          <Button
            type="button"
            variant="outline"
            disabled={busy}
            onClick={() => test.mutate()}
          >
            <ShieldCheck />
            验证仓库
          </Button>
          <Button
            type="button"
            variant="outline"
            disabled={busy || !initial.override_configured}
            onClick={() => reset.mutate()}
          >
            <RotateCcw />
            恢复环境变量
          </Button>
        </div>
      </form>
    </FormCard>
  );
}

function FormCard({
  title,
  description,
  icon,
  source,
  children,
}: {
  title: string;
  description: string;
  icon: ReactNode;
  source: string;
  children: ReactNode;
}) {
  return (
    <Card>
      <CardHeader className="flex-row items-start justify-between gap-4">
        <div>
          <div className="mb-2 flex items-center gap-2 text-muted-foreground">
            {icon}
            <CardTitle className="text-base text-foreground">{title}</CardTitle>
          </div>
          <CardDescription>{description}</CardDescription>
        </div>
        <StatusBadge
          status={source.includes("环境") ? "pending" : "succeeded"}
          label={source}
        />
      </CardHeader>
      <CardContent>{children}</CardContent>
    </Card>
  );
}
function Field({
  label,
  id,
  children,
}: {
  label: string;
  id: string;
  children: ReactNode;
}) {
  return (
    <div className="grid gap-2">
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  );
}
function sourceLabel(source: string) {
  return source === "database"
    ? "控制台设置"
    : source === "environment"
      ? "环境变量"
      : "未配置";
}
function split(value: string) {
  return value
    .split(",")
    .map((item) => item.trim())
    .filter(Boolean);
}
