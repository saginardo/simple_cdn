import { useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { AuthScreen } from "@/features/auth/auth-screen";
import { api, setCsrfToken } from "@/lib/api";
import { t } from "@/lib/i18n";
type AuthStage = "boot" | "setup" | "login" | "authenticated" | "error";
interface SetupResult {
  totp_secret: string;
  otpauth_url: string;
  recovery_codes: string[];
}
interface AuthContextValue {
  user: string;
  login: (input: {
    password: string;
    totp: string;
    recovery_code: string;
  }) => Promise<void>;
  logout: () => Promise<void>;
}
const AuthContext = createContext<AuthContextValue | null>(null);
export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const [stage, setStage] = useState<AuthStage>("boot");
  const [user, setUser] = useState("");
  const [bootError, setBootError] = useState("");
  const [setupResult, setSetupResult] = useState<SetupResult | null>(null);
  const bootstrap = useCallback(async () => {
    setStage("boot");
    setBootError("");
    try {
      const response = await fetch("/api/session", {
        credentials: "same-origin",
      });
      if (response.ok) {
        const session = (await response.json()) as {
          user: string;
          csrf_token: string;
        };
        setCsrfToken(session.csrf_token ?? "");
        setUser(session.user ?? "admin");
        setStage("authenticated");
        return;
      }
      const status = await api<{
        initialized: boolean;
      }>("/api/setup/status");
      setStage(status.initialized ? "login" : "setup");
    } catch (error) {
      setBootError(
        error instanceof Error ? error.message : t("无法连接控制面"),
      );
      setStage("error");
    }
  }, []);
  useEffect(() => {
    void bootstrap();
  }, [bootstrap]);
  useEffect(() => {
    const unauthorized = () => {
      setCsrfToken("");
      setUser("");
      queryClient.clear();
      setStage("login");
    };
    window.addEventListener("cdn:unauthorized", unauthorized);
    return () => window.removeEventListener("cdn:unauthorized", unauthorized);
  }, [queryClient]);
  const login = useCallback(
    async (input: {
      password: string;
      totp: string;
      recovery_code: string;
    }) => {
      const result = await api<{
        csrf_token: string;
      }>("/api/login", {
        method: "POST",
        body: JSON.stringify(input),
      });
      setCsrfToken(result.csrf_token ?? "");
      setUser("admin");
      setStage("authenticated");
    },
    [],
  );
  const logout = useCallback(async () => {
    try {
      await api<{
        ok: boolean;
      }>("/api/logout", {
        method: "POST",
        body: "{}",
      });
    } finally {
      setCsrfToken("");
      setUser("");
      queryClient.clear();
      setStage("login");
    }
  }, [queryClient]);
  const setup = useCallback(async (password: string) => {
    const result = await api<SetupResult>("/api/setup", {
      method: "POST",
      body: JSON.stringify({
        password,
      }),
    });
    setSetupResult(result);
  }, []);
  const value = useMemo(
    () => ({
      user,
      login,
      logout,
    }),
    [user, login, logout],
  );
  return (
    <AuthContext.Provider value={value}>
      {stage === "authenticated" ? (
        children
      ) : (
        <AuthScreen
          stage={stage}
          error={bootError}
          setupResult={setupResult}
          onRetry={bootstrap}
          onSetup={setup}
          onSetupComplete={() => {
            setSetupResult(null);
            setStage("login");
          }}
          onLogin={login}
        />
      )}
    </AuthContext.Provider>
  );
}
export function AuthGate({ children }: { children: ReactNode }) {
  return <>{children}</>;
}
export function useAuth() {
  const context = useContext(AuthContext);
  if (!context) throw new Error("useAuth must be used within AuthProvider");
  return context;
}
