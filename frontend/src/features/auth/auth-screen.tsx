import {
  AlertCircle,
  Check,
  ExternalLink,
  Fingerprint,
  LoaderCircle,
  LockKeyhole,
  ShieldCheck,
} from "lucide-react";
import { browserSupportsWebAuthn } from "@simplewebauthn/browser";
import { useState, type FormEvent } from "react";
import { BrandMark } from "@/components/brand-mark";
import { CopyButton } from "@/components/copy-button";
import { LanguageToggle } from "@/components/language-toggle";
import { ThemeToggle } from "@/components/theme-toggle";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
} from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Separator } from "@/components/ui/separator";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { DEFAULT_BRANDING, useCachedBranding } from "@/hooks/use-branding";
import { errorMessage } from "@/lib/api";
import { t, useI18n } from "@/lib/i18n";
interface SetupResult {
  totp_secret: string;
  otpauth_url: string;
  recovery_codes: string[];
}
export function AuthScreen({
  stage,
  error,
  setupResult,
  passkeyEnabled,
  onRetry,
  onSetup,
  onSetupConfirm,
  onLogin,
  onPasskeyLogin,
}: {
  stage: "boot" | "setup" | "login" | "authenticated" | "error";
  error: string;
  setupResult: SetupResult | null;
  passkeyEnabled: boolean;
  onRetry: () => Promise<void>;
  onSetup: (initializationToken: string, password: string) => Promise<void>;
  onSetupConfirm: (
    initializationToken: string,
    password: string,
    totpCode: string,
  ) => Promise<void>;
  onLogin: (input: {
    password: string;
    totp: string;
    recovery_code: string;
  }) => Promise<void>;
  onPasskeyLogin: () => Promise<void>;
}) {
  useI18n();
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [initializationToken, setInitializationToken] = useState("");
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [totp, setTotp] = useState("");
  const [recoveryCode, setRecoveryCode] = useState("");
  const [factor, setFactor] = useState("totp");
  const [loginMethod, setLoginMethod] = useState("passkey");
  const [setupTOTP, setSetupTOTP] = useState("");
  const [recoveryCodesSaved, setRecoveryCodesSaved] = useState(false);
  const passkeyAvailable = passkeyEnabled && browserSupportsWebAuthn();
  const cachedBranding = useCachedBranding();
  const branding =
    cachedBranding ?? (stage === "boot" ? null : DEFAULT_BRANDING);
  async function submitSetup(event: FormEvent) {
    event.preventDefault();
    if (password !== confirmation) {
      setNotice(t("两次输入的密码不一致"));
      return;
    }
    setBusy(true);
    setNotice("");
    try {
      await onSetup(initializationToken, password);
    } catch (caught) {
      setNotice(errorMessage(caught));
    } finally {
      setBusy(false);
    }
  }
  async function submitLogin(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setNotice("");
    try {
      await onLogin({
        password,
        totp: factor === "totp" ? totp : "",
        recovery_code: factor === "recovery" ? recoveryCode : "",
      });
    } catch (caught) {
      setNotice(errorMessage(caught));
    } finally {
      setBusy(false);
    }
  }
  async function submitSetupConfirmation(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setNotice("");
    try {
      await onSetupConfirm(initializationToken, password, setupTOTP);
      setPassword("");
      setConfirmation("");
      setSetupTOTP("");
      setRecoveryCodesSaved(false);
    } catch (caught) {
      setNotice(errorMessage(caught));
    } finally {
      setBusy(false);
    }
  }
  async function submitPasskey() {
    setBusy(true);
    setNotice("");
    try {
      await onPasskeyLogin();
    } catch (caught) {
      setNotice(errorMessage(caught));
      setLoginMethod("totp");
    } finally {
      setBusy(false);
    }
  }
  return (
    <main className="relative grid min-h-svh place-items-center bg-muted/30 p-4 sm:p-8">
      <div className="absolute right-3 top-3 flex items-center gap-1 sm:right-5 sm:top-4">
        <LanguageToggle />
        <ThemeToggle />
      </div>
      <div className="w-full max-w-md">
        <div className="mb-6 flex items-center justify-center gap-3">
          <BrandMark
            logoDataURL={branding?.logo_data_url ?? ""}
            className="size-10"
            iconClassName="size-5"
          />
          {branding ? (
            <div>
              <div className="text-base font-semibold">{branding.name}</div>
              {branding.subtitle ? (
                <div className="text-xs text-muted-foreground">
                  {t(branding.subtitle)}
                </div>
              ) : null}
            </div>
          ) : (
            <div className="grid gap-1.5" aria-label={t("正在加载品牌")}>
              <div className="h-3 w-24 bg-muted" />
              <div className="h-2.5 w-16 bg-muted" />
            </div>
          )}
        </div>

        {stage === "boot" ? (
          <Card>
            <CardContent className="flex items-center justify-center gap-3 py-12 text-sm text-muted-foreground">
              <LoaderCircle className="size-4 animate-spin" />
              {t(" 正在验证登录状态")}
            </CardContent>
          </Card>
        ) : null}

        {stage === "error" ? (
          <Card>
            <CardHeader>
              <h1 className="text-lg font-semibold">{t("无法加载控制台")}</h1>
              <CardDescription>
                {error || t("控制面暂时不可用")}
              </CardDescription>
            </CardHeader>
            <CardContent>
              <Button className="w-full" onClick={() => void onRetry()}>
                {t("重试")}
              </Button>
            </CardContent>
          </Card>
        ) : null}

        {stage === "setup" && !setupResult ? (
          <Card>
            <CardHeader>
              <div className="mb-2 grid size-9 place-items-center rounded-md bg-success/10 text-success">
                <ShieldCheck className="size-5" />
              </div>
              <h1 className="text-lg font-semibold">{t("初始化控制面")}</h1>
              <CardDescription>{t("创建唯一的管理员账户")}</CardDescription>
            </CardHeader>
            <CardContent>
              <form className="grid gap-4" onSubmit={submitSetup}>
                <div className="grid gap-2">
                  <Label htmlFor="setup-initialization-token">
                    {t("一次性初始化令牌")}
                  </Label>
                  <Input
                    id="setup-initialization-token"
                    type="password"
                    required
                    autoComplete="off"
                    spellCheck={false}
                    value={initializationToken}
                    onChange={(event) =>
                      setInitializationToken(event.target.value)
                    }
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="setup-password">{t("管理员密码")}</Label>
                  <Input
                    id="setup-password"
                    type="password"
                    minLength={12}
                    required
                    autoComplete="new-password"
                    value={password}
                    onChange={(event) => setPassword(event.target.value)}
                  />
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="setup-confirmation">{t("确认密码")}</Label>
                  <Input
                    id="setup-confirmation"
                    type="password"
                    minLength={12}
                    required
                    autoComplete="new-password"
                    value={confirmation}
                    onChange={(event) => setConfirmation(event.target.value)}
                  />
                </div>
                {notice ? <InlineError message={notice} /> : null}
                <Button type="submit" disabled={busy}>
                  {busy ? (
                    <LoaderCircle className="animate-spin" />
                  ) : (
                    <LockKeyhole />
                  )}
                  {t("初始化")}
                </Button>
              </form>
            </CardContent>
          </Card>
        ) : null}

        {stage === "setup" && setupResult ? (
          <Card>
            <CardHeader>
              <div className="mb-2 grid size-9 place-items-center rounded-md bg-success/10 text-success">
                <Check className="size-5" />
              </div>
              <h1 className="text-lg font-semibold">{t("绑定管理员验证器")}</h1>
              <CardDescription>
                {t("验证 TOTP 后完成管理员创建")}
              </CardDescription>
            </CardHeader>
            <CardContent>
              <form className="grid gap-4" onSubmit={submitSetupConfirmation}>
                <div className="grid gap-2">
                  <Label>{t("TOTP 密钥")}</Label>
                  <div className="flex min-w-0 items-center gap-2">
                    <code className="min-w-0 flex-1 overflow-x-auto rounded-md border bg-muted px-3 py-2 text-xs">
                      {setupResult.totp_secret}
                    </code>
                    <CopyButton
                      value={setupResult.totp_secret}
                      label={t("复制 TOTP 密钥")}
                    />
                  </div>
                  <Button asChild type="button" variant="outline" size="sm">
                    <a href={setupResult.otpauth_url}>
                      <ExternalLink />
                      {t("在验证器中打开")}
                    </a>
                  </Button>
                </div>
                <Separator />
                <div className="grid gap-2">
                  <Label>{t("恢复代码")}</Label>
                  <div className="grid grid-cols-2 gap-1 rounded-lg border bg-muted/50 p-3 font-mono text-xs">
                    {setupResult.recovery_codes.map((code) => (
                      <span key={code}>{code}</span>
                    ))}
                  </div>
                  <div className="flex justify-end">
                    <CopyButton
                      value={setupResult.recovery_codes.join("\n")}
                      label={t("复制恢复代码")}
                    />
                  </div>
                </div>
                <div className="flex items-center gap-2">
                  <Checkbox
                    id="setup-recovery-saved"
                    checked={recoveryCodesSaved}
                    onCheckedChange={(checked) =>
                      setRecoveryCodesSaved(checked === true)
                    }
                  />
                  <Label htmlFor="setup-recovery-saved">
                    {t("恢复代码已安全保存")}
                  </Label>
                </div>
                <div className="grid gap-2">
                  <Label htmlFor="setup-totp-code">{t("6 位验证码")}</Label>
                  <Input
                    id="setup-totp-code"
                    inputMode="numeric"
                    pattern="[0-9]{6}"
                    maxLength={6}
                    required
                    autoComplete="one-time-code"
                    value={setupTOTP}
                    onChange={(event) =>
                      setSetupTOTP(event.target.value.replace(/\D/g, ""))
                    }
                  />
                </div>
                {notice ? <InlineError message={notice} /> : null}
                <Button
                  type="submit"
                  disabled={
                    busy || !recoveryCodesSaved || setupTOTP.length !== 6
                  }
                >
                  {busy ? <LoaderCircle className="animate-spin" /> : <Check />}
                  {t("确认并创建管理员")}
                </Button>
              </form>
            </CardContent>
          </Card>
        ) : null}

        {stage === "login" ? (
          <Card>
            <CardHeader>
              <h1 className="text-lg font-semibold">{t("登录控制面")}</h1>
              <CardDescription>
                {passkeyAvailable
                  ? t("使用 Passkey 或 TOTP 登录")
                  : t("使用管理员密码和双因素凭证")}
              </CardDescription>
            </CardHeader>
            <CardContent>
              {passkeyAvailable ? (
                <Tabs value={loginMethod} onValueChange={setLoginMethod}>
                  <TabsList className="grid w-full grid-cols-2">
                    <TabsTrigger value="passkey">Passkey</TabsTrigger>
                    <TabsTrigger value="totp">TOTP</TabsTrigger>
                  </TabsList>
                  <TabsContent value="passkey" className="mt-4 grid gap-4">
                    {notice ? <InlineError message={notice} /> : null}
                    <Button
                      type="button"
                      className="w-full"
                      disabled={busy}
                      onClick={() => void submitPasskey()}
                    >
                      {busy ? (
                        <LoaderCircle className="animate-spin" />
                      ) : (
                        <Fingerprint />
                      )}
                      {t("使用 Passkey 登录")}
                    </Button>
                  </TabsContent>
                  <TabsContent value="totp" className="mt-4">
                    <TOTPLoginForm
                      busy={busy}
                      notice={notice}
                      password={password}
                      setPassword={setPassword}
                      factor={factor}
                      setFactor={setFactor}
                      totp={totp}
                      setTotp={setTotp}
                      recoveryCode={recoveryCode}
                      setRecoveryCode={setRecoveryCode}
                      onSubmit={submitLogin}
                    />
                  </TabsContent>
                </Tabs>
              ) : (
                <TOTPLoginForm
                  busy={busy}
                  notice={notice}
                  password={password}
                  setPassword={setPassword}
                  factor={factor}
                  setFactor={setFactor}
                  totp={totp}
                  setTotp={setTotp}
                  recoveryCode={recoveryCode}
                  setRecoveryCode={setRecoveryCode}
                  onSubmit={submitLogin}
                />
              )}
            </CardContent>
          </Card>
        ) : null}
      </div>
    </main>
  );
}
function TOTPLoginForm({
  busy,
  notice,
  password,
  setPassword,
  factor,
  setFactor,
  totp,
  setTotp,
  recoveryCode,
  setRecoveryCode,
  onSubmit,
}: {
  busy: boolean;
  notice: string;
  password: string;
  setPassword: (value: string) => void;
  factor: string;
  setFactor: (value: string) => void;
  totp: string;
  setTotp: (value: string) => void;
  recoveryCode: string;
  setRecoveryCode: (value: string) => void;
  onSubmit: (event: FormEvent) => Promise<void>;
}) {
  return (
    <form className="grid gap-4" onSubmit={onSubmit}>
      <div className="grid gap-2">
        <Label htmlFor="login-password">{t("管理员密码")}</Label>
        <Input
          id="login-password"
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
          <Label htmlFor="login-totp">{t("6 位验证码")}</Label>
          <Input
            id="login-totp"
            inputMode="numeric"
            pattern="[0-9]{6}"
            maxLength={6}
            autoComplete="one-time-code"
            required={factor === "totp"}
            value={totp}
            onChange={(event) => setTotp(event.target.value.replace(/\D/g, ""))}
          />
        </TabsContent>
        <TabsContent value="recovery" className="mt-3 grid gap-2">
          <Label htmlFor="login-recovery">{t("恢复代码")}</Label>
          <Input
            id="login-recovery"
            autoComplete="off"
            required={factor === "recovery"}
            value={recoveryCode}
            onChange={(event) => setRecoveryCode(event.target.value)}
          />
        </TabsContent>
      </Tabs>
      {notice ? <InlineError message={notice} /> : null}
      <Button type="submit" disabled={busy}>
        {busy ? <LoaderCircle className="animate-spin" /> : <LockKeyhole />}
        {t("登录")}
      </Button>
    </form>
  );
}
function InlineError({ message }: { message: string }) {
  return (
    <Alert variant="destructive">
      <AlertCircle />
      <AlertTitle>{t("操作失败")}</AlertTitle>
      <AlertDescription>{message}</AlertDescription>
    </Alert>
  );
}
