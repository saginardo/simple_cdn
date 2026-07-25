import {
  AlertCircle,
  Check,
  LoaderCircle,
  LockKeyhole,
  ShieldCheck,
} from "lucide-react";
import { useState, type FormEvent } from "react";

import { BrandMark } from "@/components/brand-mark";
import { CopyButton } from "@/components/copy-button";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
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

interface SetupResult {
  totp_secret: string;
  otpauth_url: string;
  recovery_codes: string[];
}

export function AuthScreen({
  stage,
  error,
  setupResult,
  onRetry,
  onSetup,
  onSetupComplete,
  onLogin,
}: {
  stage: "boot" | "setup" | "login" | "authenticated" | "error";
  error: string;
  setupResult: SetupResult | null;
  onRetry: () => Promise<void>;
  onSetup: (password: string) => Promise<void>;
  onSetupComplete: () => void;
  onLogin: (input: {
    password: string;
    totp: string;
    recovery_code: string;
  }) => Promise<void>;
}) {
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [password, setPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [totp, setTotp] = useState("");
  const [recoveryCode, setRecoveryCode] = useState("");
  const [factor, setFactor] = useState("totp");
  const cachedBranding = useCachedBranding();
  const branding =
    cachedBranding ?? (stage === "boot" ? null : DEFAULT_BRANDING);

  async function submitSetup(event: FormEvent) {
    event.preventDefault();
    if (password !== confirmation) {
      setNotice("两次输入的密码不一致");
      return;
    }
    setBusy(true);
    setNotice("");
    try {
      await onSetup(password);
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

  return (
    <main className="grid min-h-svh place-items-center bg-muted/30 p-4 sm:p-8">
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
                  {branding.subtitle}
                </div>
              ) : null}
            </div>
          ) : (
            <div className="grid gap-1.5" aria-label="正在加载品牌">
              <div className="h-3 w-24 bg-muted" />
              <div className="h-2.5 w-16 bg-muted" />
            </div>
          )}
        </div>

        {stage === "boot" ? (
          <Card>
            <CardContent className="flex items-center justify-center gap-3 py-12 text-sm text-muted-foreground">
              <LoaderCircle className="size-4 animate-spin" /> 正在验证登录状态
            </CardContent>
          </Card>
        ) : null}

        {stage === "error" ? (
          <Card>
            <CardHeader>
              <h1 className="text-lg font-semibold">无法加载控制台</h1>
              <CardDescription>{error || "控制面暂时不可用"}</CardDescription>
            </CardHeader>
            <CardContent>
              <Button className="w-full" onClick={() => void onRetry()}>
                重试
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
              <h1 className="text-lg font-semibold">初始化控制面</h1>
              <CardDescription>创建唯一的管理员账户</CardDescription>
            </CardHeader>
            <CardContent>
              <form className="grid gap-4" onSubmit={submitSetup}>
                <div className="grid gap-2">
                  <Label htmlFor="setup-password">管理员密码</Label>
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
                  <Label htmlFor="setup-confirmation">确认密码</Label>
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
                  初始化
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
              <h1 className="text-lg font-semibold">管理员已创建</h1>
              <CardDescription>保存 TOTP 密钥与恢复代码后登录</CardDescription>
            </CardHeader>
            <CardContent className="grid gap-4">
              <div className="grid gap-2">
                <Label>TOTP 密钥</Label>
                <div className="flex min-w-0 items-center gap-2">
                  <code className="min-w-0 flex-1 overflow-x-auto rounded-md border bg-muted px-3 py-2 text-xs">
                    {setupResult.totp_secret}
                  </code>
                  <CopyButton
                    value={setupResult.totp_secret}
                    label="复制 TOTP 密钥"
                  />
                </div>
              </div>
              <Separator />
              <div className="grid gap-2">
                <Label>恢复代码</Label>
                <div className="grid grid-cols-2 gap-1 rounded-lg border bg-muted/50 p-3 font-mono text-xs">
                  {setupResult.recovery_codes.map((code) => (
                    <span key={code}>{code}</span>
                  ))}
                </div>
                <div className="flex justify-end">
                  <CopyButton
                    value={setupResult.recovery_codes.join("\n")}
                    label="复制恢复代码"
                  />
                </div>
              </div>
              <Button onClick={onSetupComplete}>前往登录</Button>
            </CardContent>
          </Card>
        ) : null}

        {stage === "login" ? (
          <Card>
            <CardHeader>
              <h1 className="text-lg font-semibold">登录控制面</h1>
              <CardDescription>使用管理员密码和双因素凭证</CardDescription>
            </CardHeader>
            <CardContent>
              <form className="grid gap-4" onSubmit={submitLogin}>
                <div className="grid gap-2">
                  <Label htmlFor="login-password">管理员密码</Label>
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
                    <TabsTrigger value="recovery">恢复代码</TabsTrigger>
                  </TabsList>
                  <TabsContent value="totp" className="mt-3 grid gap-2">
                    <Label htmlFor="login-totp">6 位验证码</Label>
                    <Input
                      id="login-totp"
                      inputMode="numeric"
                      pattern="[0-9]{6}"
                      maxLength={6}
                      autoComplete="one-time-code"
                      required={factor === "totp"}
                      value={totp}
                      onChange={(event) =>
                        setTotp(event.target.value.replace(/\D/g, ""))
                      }
                    />
                  </TabsContent>
                  <TabsContent value="recovery" className="mt-3 grid gap-2">
                    <Label htmlFor="login-recovery">恢复代码</Label>
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
                  {busy ? (
                    <LoaderCircle className="animate-spin" />
                  ) : (
                    <LockKeyhole />
                  )}
                  登录
                </Button>
              </form>
            </CardContent>
          </Card>
        ) : null}
      </div>
    </main>
  );
}

function InlineError({ message }: { message: string }) {
  return (
    <Alert variant="destructive">
      <AlertCircle />
      <AlertTitle>操作失败</AlertTitle>
      <AlertDescription>{message}</AlertDescription>
    </Alert>
  );
}
