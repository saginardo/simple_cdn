import { t } from "@/lib/i18n";
let csrfToken = "";
export class ApiError extends Error {
  status: number;
  data: unknown;
  constructor(message: string, status: number, data: unknown) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.data = data;
  }
}
export function setCsrfToken(token: string) {
  csrfToken = token;
}
export function jsonBody(value: unknown): Pick<RequestInit, "body"> {
  return {
    body: JSON.stringify(value),
  };
}
export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const method = (init.method ?? "GET").toUpperCase();
  const headers = new Headers(init.headers);
  const multipart =
    typeof FormData !== "undefined" && init.body instanceof FormData;
  if (init.body != null && !multipart && !headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json");
  }
  if (method !== "GET" && method !== "HEAD" && csrfToken) {
    headers.set("X-CSRF-Token", csrfToken);
  }
  const response = await fetch(path, {
    ...init,
    method,
    headers,
    credentials: "same-origin",
  });
  const data = await response.json().catch(() => undefined);
  if (!response.ok) {
    const message =
      data && typeof data === "object" && "error" in data
        ? String(data.error)
        : t("请求失败（HTTP {value0}）", {
            value0: response.status,
          });
    if (response.status === 401) {
      window.dispatchEvent(new CustomEvent("cdn:unauthorized"));
    }
    throw new ApiError(message, response.status, data);
  }
  return data as T;
}
export function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : t("发生未知错误");
}
