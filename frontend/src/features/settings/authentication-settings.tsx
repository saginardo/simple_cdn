import {
  browserSupportsWebAuthn,
  startRegistration,
  type PublicKeyCredentialCreationOptionsJSON,
} from "@simplewebauthn/browser";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  ExternalLink,
  Fingerprint,
  KeyRound,
  LoaderCircle,
  LockKeyhole,
  Plus,
  RefreshCw,
  ShieldCheck,
  Trash2,
} from "lucide-react";
import { useState, type FormEvent } from "react";
import { toast } from "sonner";
import { ConfirmDialog } from "@/components/confirm-dialog";
import { CopyButton } from "@/components/copy-button";
import { PageError, PageLoading } from "@/components/page";
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
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api, ApiError, errorMessage } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import type { AuthenticationSettings, PasskeyCredential } from "@/lib/types";

interface TOTPSetup {
  totp_secret: string;
  otpauth_url: string;
}

const authenticationQueryKey = ["authentication-settings"] as const;

export function AuthenticationSettingsPanel() {
  const [reauthenticationOpen, setReauthenticationOpen] = useState(false);
  const query = useQuery({
    queryKey: authenticationQueryKey,
    queryFn: () => api<AuthenticationSettings>("/api/settings/authentication"),
  });
  if (query.isLoading) return <PageLoading />;
  if (query.error) return <PageError error={query.error} />;
  if (!query.data) return null;
  return (
    <div className="grid gap-4">
      {!query.data.recent_authentication ? (
        <Alert>
          <LockKeyhole />
          <AlertTitle>{t("安全设置已锁定")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
            <span>{t("重新验证管理员身份后可以修改登录凭据")}</span>
            <Button
              type="button"
              size="sm"
              onClick={() => setReauthenticationOpen(true)}
            >
              <LockKeyhole />
              {t("重新验证")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      <div className="grid gap-4 lg:grid-cols-2">
        <TOTPSettings
          recentAuthentication={query.data.recent_authentication}
          onReauthenticationRequired={() => setReauthenticationOpen(true)}
        />
        <PasskeySettings
          settings={query.data}
          onReauthenticationRequired={() => setReauthenticationOpen(true)}
        />
      </div>
      <ReauthenticationDialog
        open={reauthenticationOpen}
        onOpenChange={setReauthenticationOpen}
        onSuccess={() => void query.refetch()}
      />
    </div>
  );
}

function TOTPSettings({
  recentAuthentication,
  onReauthenticationRequired,
}: {
  recentAuthentication: boolean;
  onReauthenticationRequired: () => void;
}) {
  const [setup, setSetup] = useState<TOTPSetup | null>(null);
  const [code, setCode] = useState("");
  const begin = useMutation({
    mutationFn: () =>
      api<TOTPSetup>("/api/settings/authentication/totp/begin", {
        method: "POST",
        body: "{}",
      }),
    onSuccess: setSetup,
    onError: (error) =>
      handleAuthenticationMutationError(error, onReauthenticationRequired),
  });
  const confirm = useMutation({
    mutationFn: () =>
      api<{ totp_enabled: true }>("/api/settings/authentication/totp", {
        method: "PUT",
        body: JSON.stringify({
          totp_secret: setup?.totp_secret,
          code,
        }),
      }),
    onSuccess: () => {
      setSetup(null);
      setCode("");
      toast.success(t("TOTP 已更换"));
    },
    onError: (error) =>
      handleAuthenticationMutationError(error, onReauthenticationRequired),
  });
  return (
    <Card>
      <CardHeader className="flex-row items-start justify-between gap-4">
        <div>
          <div className="mb-2 flex items-center gap-2 text-muted-foreground">
            <KeyRound />
            <CardTitle className="text-base text-foreground">TOTP</CardTitle>
          </div>
          <CardDescription>{t("管理员登录备用验证方式")}</CardDescription>
        </div>
        <StatusBadge status="active" label={t("始终开启")} />
      </CardHeader>
      <CardContent className="grid gap-4">
        <div className="flex items-center justify-between gap-4 rounded-md border px-3 py-3">
          <Label htmlFor="totp-always-enabled">{t("TOTP 登录")}</Label>
          <Switch
            id="totp-always-enabled"
            checked
            disabled
            aria-label={t("TOTP 始终开启")}
          />
        </div>
        <div>
          <Button
            type="button"
            variant="outline"
            disabled={begin.isPending || !recentAuthentication}
            onClick={() => begin.mutate()}
          >
            {begin.isPending ? (
              <LoaderCircle className="animate-spin" />
            ) : (
              <RefreshCw />
            )}
            {t("更换 TOTP")}
          </Button>
        </div>
      </CardContent>
      <Dialog
        open={setup !== null}
        onOpenChange={(open) => {
          if (!open && !confirm.isPending) {
            setSetup(null);
            setCode("");
          }
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("设置新的 TOTP")}</DialogTitle>
            <DialogDescription>
              {t("使用验证器添加新密钥并输入生成的验证码")}
            </DialogDescription>
          </DialogHeader>
          {setup ? (
            <form
              className="grid gap-4"
              onSubmit={(event: FormEvent) => {
                event.preventDefault();
                confirm.mutate();
              }}
            >
              <div className="grid gap-2">
                <Label>{t("新 TOTP 密钥")}</Label>
                <div className="flex min-w-0 items-center gap-2">
                  <code className="min-w-0 flex-1 overflow-x-auto rounded-md border bg-muted px-3 py-2 text-xs">
                    {setup.totp_secret}
                  </code>
                  <CopyButton
                    value={setup.totp_secret}
                    label={t("复制新 TOTP 密钥")}
                  />
                </div>
                <Button asChild type="button" variant="outline" size="sm">
                  <a href={setup.otpauth_url}>
                    <ExternalLink />
                    {t("在验证器中打开")}
                  </a>
                </Button>
              </div>
              <div className="grid gap-2">
                <Label htmlFor="totp-change-code">{t("新 6 位验证码")}</Label>
                <Input
                  id="totp-change-code"
                  inputMode="numeric"
                  pattern="[0-9]{6}"
                  maxLength={6}
                  required
                  autoComplete="one-time-code"
                  value={code}
                  onChange={(event) =>
                    setCode(event.target.value.replace(/\D/g, ""))
                  }
                />
              </div>
              <DialogFooter>
                <Button
                  type="submit"
                  disabled={confirm.isPending || code.length !== 6}
                >
                  {confirm.isPending ? (
                    <LoaderCircle className="animate-spin" />
                  ) : (
                    <ShieldCheck />
                  )}
                  {t("确认更换")}
                </Button>
              </DialogFooter>
            </form>
          ) : null}
        </DialogContent>
      </Dialog>
    </Card>
  );
}

function handleAuthenticationMutationError(
  error: unknown,
  onReauthenticationRequired: () => void,
) {
  if (error instanceof ApiError && error.status === 428) {
    onReauthenticationRequired();
    return;
  }
  toast.error(errorMessage(error));
}

function ReauthenticationDialog({
  open,
  onOpenChange,
  onSuccess,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSuccess: () => void;
}) {
  const [password, setPassword] = useState("");
  const [factor, setFactor] = useState("totp");
  const [totp, setTOTP] = useState("");
  const [recoveryCode, setRecoveryCode] = useState("");
  const reset = () => {
    setPassword("");
    setFactor("totp");
    setTOTP("");
    setRecoveryCode("");
  };
  const confirm = useMutation({
    mutationFn: () =>
      api<{ ok: true; elevated_until: string }>(
        "/api/settings/authentication/reauthenticate",
        {
          method: "POST",
          body: JSON.stringify({
            password,
            totp: factor === "totp" ? totp : "",
            recovery_code: factor === "recovery" ? recoveryCode : "",
          }),
        },
      ),
    onSuccess: () => {
      reset();
      onOpenChange(false);
      onSuccess();
      toast.success(t("管理员身份已重新验证"));
    },
    onError: (error) => toast.error(errorMessage(error)),
  });
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!next && !confirm.isPending) reset();
        onOpenChange(next);
      }}
    >
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t("重新验证管理员身份")}</DialogTitle>
          <DialogDescription>
            {t("安全设置需要当前密码和现有双因素凭据")}
          </DialogDescription>
        </DialogHeader>
        <form
          className="grid gap-4"
          onSubmit={(event: FormEvent) => {
            event.preventDefault();
            confirm.mutate();
          }}
        >
          <div className="grid gap-2">
            <Label htmlFor="reauthentication-password">{t("管理员密码")}</Label>
            <Input
              id="reauthentication-password"
              type="password"
              required
              autoComplete="current-password"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
            />
          </div>
          <Tabs value={factor} onValueChange={setFactor}>
            <TabsList className="grid w-full grid-cols-2">
              <TabsTrigger value="totp">TOTP</TabsTrigger>
              <TabsTrigger value="recovery">{t("恢复代码")}</TabsTrigger>
            </TabsList>
            <TabsContent value="totp" className="mt-3 grid gap-2">
              <Label htmlFor="reauthentication-totp">{t("6 位验证码")}</Label>
              <Input
                id="reauthentication-totp"
                inputMode="numeric"
                pattern="[0-9]{6}"
                maxLength={6}
                required={factor === "totp"}
                autoComplete="one-time-code"
                value={totp}
                onChange={(event) =>
                  setTOTP(event.target.value.replace(/\D/g, ""))
                }
              />
            </TabsContent>
            <TabsContent value="recovery" className="mt-3 grid gap-2">
              <Label htmlFor="reauthentication-recovery">{t("恢复代码")}</Label>
              <Input
                id="reauthentication-recovery"
                required={factor === "recovery"}
                autoComplete="off"
                value={recoveryCode}
                onChange={(event) => setRecoveryCode(event.target.value)}
              />
            </TabsContent>
          </Tabs>
          <DialogFooter>
            <Button
              type="submit"
              disabled={
                confirm.isPending ||
                !password ||
                (factor === "totp" ? totp.length !== 6 : !recoveryCode.trim())
              }
            >
              {confirm.isPending ? (
                <LoaderCircle className="animate-spin" />
              ) : (
                <ShieldCheck />
              )}
              {t("重新验证")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function PasskeySettings({
  settings,
  onReauthenticationRequired,
}: {
  settings: AuthenticationSettings;
  onReauthenticationRequired: () => void;
}) {
  const queryClient = useQueryClient();
  const supported = browserSupportsWebAuthn();
  const [name, setName] = useState(t("管理员 Passkey"));
  const [registering, setRegistering] = useState(false);
  const [deleteTarget, setDeleteTarget] = useState<PasskeyCredential | null>(
    null,
  );
  const updateSettings = (next: AuthenticationSettings) =>
    queryClient.setQueryData(authenticationQueryKey, next);
  const toggle = useMutation({
    mutationFn: (enabled: boolean) =>
      api<AuthenticationSettings>("/api/settings/authentication/passkeys", {
        method: "PUT",
        body: JSON.stringify({ enabled }),
      }),
    onSuccess: (next) => {
      updateSettings(next);
      toast.success(
        next.passkey_enabled
          ? t("Passkey 登录已开启")
          : t("Passkey 登录已关闭"),
      );
    },
    onError: (error) =>
      handleAuthenticationMutationError(error, onReauthenticationRequired),
  });
  const remove = useMutation({
    mutationFn: (credential: PasskeyCredential) =>
      api<AuthenticationSettings>(
        `/api/settings/authentication/passkeys/${encodeURIComponent(credential.id)}?rp_id=${encodeURIComponent(credential.rp_id)}`,
        { method: "DELETE" },
      ),
    onSuccess: (next) => {
      updateSettings(next);
      setDeleteTarget(null);
      toast.success(t("Passkey 已删除"));
    },
    onError: (error) =>
      handleAuthenticationMutationError(error, onReauthenticationRequired),
  });
  async function registerPasskey() {
    const trimmedName = name.trim();
    if (!trimmedName) return;
    setRegistering(true);
    try {
      const options = await api<PublicKeyCredentialCreationOptionsJSON>(
        "/api/settings/authentication/passkeys/begin",
        {
          method: "POST",
          body: JSON.stringify({ name: trimmedName }),
        },
      );
      const credential = await startRegistration({ optionsJSON: options });
      const next = await api<AuthenticationSettings>(
        "/api/settings/authentication/passkeys/finish",
        {
          method: "POST",
          body: JSON.stringify(credential),
        },
      );
      updateSettings(next);
      setName(t("管理员 Passkey"));
      toast.success(t("Passkey 已添加并开启登录"));
    } catch (error) {
      handleAuthenticationMutationError(error, onReauthenticationRequired);
    } finally {
      setRegistering(false);
    }
  }
  const unavailable = !settings.passkey_available || !supported;
  const hasCurrentPasskey = settings.passkeys.some(
    (credential) => credential.current_rp,
  );
  return (
    <Card>
      <CardHeader className="flex-row items-start justify-between gap-4">
        <div>
          <div className="mb-2 flex items-center gap-2 text-muted-foreground">
            <Fingerprint />
            <CardTitle className="text-base text-foreground">Passkey</CardTitle>
          </div>
          <CardDescription>{t("无密码管理员登录")}</CardDescription>
        </div>
        <StatusBadge
          status={settings.passkey_operational ? "active" : "pending"}
          label={
            settings.passkey_operational
              ? t("已开启")
              : settings.passkey_enabled
                ? t("当前域名待绑定")
                : t("已关闭")
          }
        />
      </CardHeader>
      <CardContent className="grid gap-4">
        {settings.passkey_error ? (
          <Alert variant="destructive">
            <AlertTitle>{t("Passkey 不可用")}</AlertTitle>
            <AlertDescription>{settings.passkey_error}</AlertDescription>
          </Alert>
        ) : null}
        {settings.passkey_enabled && !settings.passkey_operational ? (
          <Alert>
            <AlertTitle>{t("当前域名无法使用 Passkey")}</AlertTitle>
            <AlertDescription>
              {t("为当前域名添加 Passkey，或关闭全局 Passkey 登录")}
            </AlertDescription>
          </Alert>
        ) : null}
        <div className="flex items-center justify-between gap-4 rounded-md border px-3 py-3">
          <div className="min-w-0">
            <Label htmlFor="passkey-enabled">{t("Passkey 登录")}</Label>
            {settings.rp_id ? (
              <p className="truncate text-xs text-muted-foreground">
                {settings.rp_id}
              </p>
            ) : null}
          </div>
          <Switch
            id="passkey-enabled"
            checked={settings.passkey_enabled}
            disabled={
              toggle.isPending ||
              !settings.recent_authentication ||
              (!settings.passkey_enabled &&
                (!settings.passkey_available || !hasCurrentPasskey))
            }
            onCheckedChange={(enabled) => toggle.mutate(enabled)}
          />
        </div>
        <div className="grid gap-2 sm:grid-cols-[1fr_auto]">
          <div className="grid gap-2">
            <Label htmlFor="passkey-name">{t("Passkey 名称")}</Label>
            <Input
              id="passkey-name"
              maxLength={64}
              value={name}
              onChange={(event) => setName(event.target.value)}
            />
          </div>
          <Button
            type="button"
            className="self-end"
            disabled={
              unavailable ||
              registering ||
              !name.trim() ||
              !settings.recent_authentication
            }
            onClick={() => void registerPasskey()}
          >
            {registering ? <LoaderCircle className="animate-spin" /> : <Plus />}
            {t("添加 Passkey")}
          </Button>
        </div>
        <div className="divide-y rounded-md border">
          {settings.passkeys.length ? (
            settings.passkeys.map((credential) => (
              <div
                key={credential.id}
                className="flex min-w-0 items-center gap-3 px-3 py-3"
              >
                <Fingerprint className="size-4 shrink-0 text-muted-foreground" />
                <div className="min-w-0 flex-1">
                  <div className="truncate font-medium">{credential.name}</div>
                  <div className="truncate text-xs text-muted-foreground">
                    {credential.rp_id}
                    {credential.current_rp ? ` · ${t("当前域名")}` : ""}
                  </div>
                  <div className="truncate text-xs text-muted-foreground">
                    {credential.last_used_at
                      ? t("最近使用 {value0}", {
                          value0: formatDateTime(credential.last_used_at),
                        })
                      : t("添加于 {value0}", {
                          value0: formatDateTime(credential.created_at),
                        })}
                  </div>
                </div>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  aria-label={t("删除 Passkey {value0}", {
                    value0: credential.name,
                  })}
                  disabled={!settings.recent_authentication}
                  onClick={() => setDeleteTarget(credential)}
                >
                  <Trash2 />
                </Button>
              </div>
            ))
          ) : (
            <p className="px-3 py-5 text-center text-sm text-muted-foreground">
              {t("尚未添加 Passkey")}
            </p>
          )}
        </div>
      </CardContent>
      <ConfirmDialog
        open={deleteTarget !== null}
        onOpenChange={(open) => !open && setDeleteTarget(null)}
        title={t("删除 Passkey")}
        description={t("删除后，此 Passkey 将无法再用于登录。")}
        confirmLabel={t("删除")}
        busy={remove.isPending}
        destructive
        onConfirm={() => {
          if (deleteTarget) {
            return remove.mutateAsync(deleteTarget).then(() => undefined);
          }
        }}
      />
    </Card>
  );
}
