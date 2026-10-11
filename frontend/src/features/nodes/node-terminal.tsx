import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { TerminalSquare, Unplug } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { ReauthenticationDialog } from "@/features/settings/authentication-settings";
import { api, ApiError, errorMessage } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { t } from "@/lib/i18n";
import type { Node } from "@/lib/types";

interface TerminalSession {
  id: string;
  actor: string;
  remote_addr: string;
  user: string;
  shell: string;
  status: string;
  reason: string;
  created_at: string;
  started_at?: string;
  ended_at?: string;
}
interface TerminalStatus {
  connected: boolean;
  user: string;
  shell: string;
  sessions: TerminalSession[];
  idle_timeout_seconds: number;
  max_duration_seconds: number;
}

function reasonLabel(reason: string) {
  const reasons: Record<string, string> = {
    user_closed: t("手动关闭"),
    browser_disconnected: t("浏览器连接已断开"),
    agent_disconnected: t("节点连接已断开"),
    idle_timeout: t("空闲超时"),
    max_duration: t("达到会话时限"),
    connect_timeout: t("连接超时"),
    connect_failed: t("连接失败"),
    authentication_expired: t("登录已失效"),
    node_unauthorized: t("节点授权已失效"),
    control_restarted: t("主控重启"),
    control_stopped: t("主控已停止"),
    shell_exited: t("Shell 已退出"),
    shell_failed: t("Shell 启动失败"),
    io_error: t("终端读写失败"),
    invalid_message: t("终端协议错误"),
    audit_failed: t("会话审计写入失败"),
    control_error: t("控制面错误"),
  };
  return reasons[reason] ?? reason;
}

export function NodeTerminal({ node }: { node: Node }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button variant="outline" onClick={() => setOpen(true)}>
        <TerminalSquare />
        {t("终端")}
      </Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent
          className="max-h-[95dvh] w-[calc(100%-2rem)] overflow-y-auto sm:max-w-6xl"
          onEscapeKeyDown={(event) => {
            // Escape belongs to interactive programs (for example vim).
            if (
              event.target instanceof Element &&
              event.target.closest(".xterm")
            ) {
              event.preventDefault();
            }
          }}
        >
          <DialogHeader>
            <DialogTitle>
              {t("节点终端")} · {node.name}
            </DialogTitle>
            <DialogDescription>
              {node.public_ipv4} · {t("关闭窗口将结束会话")}
            </DialogDescription>
          </DialogHeader>
          {open ? <TerminalPanel key={node.id} node={node} /> : null}
        </DialogContent>
      </Dialog>
    </>
  );
}

function TerminalPanel({ node }: { node: Node }) {
  const queryClient = useQueryClient();
  const base = `/api/nodes/${encodeURIComponent(node.id)}/terminal-sessions`;
  const queryKey = ["terminal-sessions", node.id];
  const status = useQuery({
    queryKey,
    queryFn: () => api<TerminalStatus>(base),
    refetchInterval: 5000,
  });
  const container = useRef<HTMLDivElement>(null);
  const terminal = useRef<Terminal | null>(null);
  const socket = useRef<WebSocket | null>(null);
  const sessionRef = useRef<string | null>(null);
  const generation = useRef(0);
  const [ready, setReady] = useState(false);
  const [busy, setBusy] = useState(false);
  const [session, setSession] = useState<string | null>(null);
  const [connection, setConnection] = useState("");
  const [reauth, setReauth] = useState(false);
  const refresh = () => void queryClient.invalidateQueries({ queryKey });

  useEffect(() => {
    const instance = new Terminal({
      cursorBlink: true,
      fontSize: 14,
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
      scrollback: 3000,
      theme: { background: "#09090b", foreground: "#fafafa" },
      allowProposedApi: false,
    });
    const fit = new FitAddon();
    instance.loadAddon(fit);
    if (!container.current) return;
    instance.open(container.current);
    terminal.current = instance;
    const resize = () => {
      if (container.current?.clientWidth) {
        fit.fit();
      }
    };
    const observer = new ResizeObserver(resize);
    observer.observe(container.current);
    resize();
    setReady(true);
    const send = (message: object) => {
      const ws = socket.current;
      if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify(message));
    };
    const data = instance.onData((text) => {
      const bytes = new TextEncoder().encode(text);
      for (let offset = 0; offset < bytes.length; offset += 8192) {
        if ((socket.current?.bufferedAmount ?? 0) > 1_048_576) {
          socket.current?.close();
          break;
        }
        send({
          type: "input",
          data: btoa(
            String.fromCharCode(...bytes.subarray(offset, offset + 8192)),
          ),
        });
      }
    });
    const binary = instance.onBinary((text) =>
      send({ type: "input", data: btoa(text) }),
    );
    const size = instance.onResize(({ cols, rows }) =>
      send({
        type: "resize",
        cols: Math.min(500, Math.max(2, cols)),
        rows: Math.min(200, Math.max(2, rows)),
      }),
    );
    return () => {
      generation.current++;
      observer.disconnect();
      data.dispose();
      binary.dispose();
      size.dispose();
      const ws = socket.current;
      socket.current = null;
      ws?.close();
      if (sessionRef.current)
        void api(`${base}/${sessionRef.current}`, { method: "DELETE" }).catch(
          () => {},
        );
      sessionRef.current = null;
      terminal.current = null;
      instance.dispose();
    };
  }, [base]);

  async function connect() {
    if (!terminal.current || busy || sessionRef.current) return;
    setBusy(true);
    setConnection(t("正在连接"));
    const currentGeneration = generation.current;
    try {
      const created = await api<TerminalSession>(base, {
        method: "POST",
        body: JSON.stringify({
          cols: Math.min(500, Math.max(2, terminal.current.cols)),
          rows: Math.min(200, Math.max(2, terminal.current.rows)),
        }),
      });
      if (currentGeneration !== generation.current) {
        void api(`${base}/${created.id}`, { method: "DELETE" }).catch(() => {});
        return;
      }
      sessionRef.current = created.id;
      setSession(created.id);
      terminal.current?.reset();
      const url = new URL(
        `${base}/${created.id}/connect`,
        window.location.href,
      );
      url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
      const ws = new WebSocket(url);
      socket.current = ws;
      let ended = false;
      let pendingOutput = 0;
      ws.onmessage = (event: MessageEvent<string>) => {
        if (socket.current !== ws) return;
        try {
          const message = JSON.parse(event.data) as {
            type: string;
            data?: string;
            reason?: string;
          };
          if (message.type === "ready") {
            setConnection(t("已连接"));
            terminal.current?.focus();
            refresh();
          }
          if (message.type === "output" && message.data) {
            const bytes = Uint8Array.from(atob(message.data), (char) =>
              char.charCodeAt(0),
            );
            pendingOutput += bytes.length;
            if (pendingOutput > 1_048_576) {
              setConnection(t("终端输出过快，连接已关闭"));
              ended = true;
              ws.close();
              return;
            }
            terminal.current?.write(bytes, () => {
              pendingOutput -= bytes.length;
            });
          }
          if (message.type === "closed") {
            ended = true;
            setConnection(reasonLabel(message.reason ?? ""));
            ws.close();
          }
        } catch {
          ws.close();
        }
      };
      ws.onclose = () => {
        if (socket.current !== ws) return;
        socket.current = null;
        sessionRef.current = null;
        setSession(null);
        if (!ended) setConnection(t("浏览器连接已断开"));
        refresh();
      };
      ws.onerror = () => {
        if (socket.current === ws) setConnection(t("连接失败"));
      };
      refresh();
    } catch (error) {
      if (currentGeneration !== generation.current) return;
      if (error instanceof ApiError && error.status === 428) {
        setReauth(true);
        setConnection(t("请重新验证管理员身份"));
      } else {
        setConnection(errorMessage(error));
        toast.error(errorMessage(error));
      }
    } finally {
      if (currentGeneration === generation.current) setBusy(false);
    }
  }

  async function close(id: string) {
    try {
      await api(`${base}/${id}`, { method: "DELETE" });
      refresh();
    } catch (error) {
      toast.error(errorMessage(error));
    }
  }
  const active = status.data?.sessions.find((item) => !item.ended_at);
  const capable = node.capabilities.includes("terminal_v1");
  return (
    <div className="grid min-w-0 gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="text-sm text-muted-foreground">
          {status.data?.user ? (
            <span>
              {t("执行用户")}：
              <strong className="text-foreground">{status.data.user}</strong> ·{" "}
              {status.data.shell}
            </span>
          ) : null}
          <p>
            {t("连续 {idle} 分钟无输入或累计 {maximum} 分钟后关闭", {
              idle: (status.data?.idle_timeout_seconds ?? 600) / 60,
              maximum: (status.data?.max_duration_seconds ?? 3600) / 60,
            })}
          </p>
        </div>
        {session || active ? (
          <Button
            variant="outline"
            onClick={() => void close(session ?? active!.id)}
          >
            <Unplug />
            {t("结束会话")}
          </Button>
        ) : (
          <Button
            onClick={() => void connect()}
            disabled={!ready || busy || !capable || !status.data?.connected}
          >
            {t("连接终端")}
          </Button>
        )}
      </div>
      <p role="status" className="text-sm">
        {connection ||
          (!capable
            ? t("请升级节点 Agent 以启用终端")
            : status.error
              ? errorMessage(status.error)
              : status.data?.connected
                ? t("终端就绪")
                : t("节点终端离线"))}
      </p>
      <div
        ref={container}
        aria-label={t("交互终端")}
        className="h-[min(50dvh,440px)] min-h-60 min-w-0 overflow-hidden rounded-md bg-zinc-950 p-2"
      />
      <div className="grid gap-2">
        <h3 className="text-sm font-medium">{t("会话审计（最近 50 条）")}</h3>
        <div className="max-h-48 overflow-auto rounded-md border">
          <table className="w-full text-left text-xs">
            <thead className="bg-muted">
              <tr>
                {[
                  t("时间"),
                  t("操作者 / 来源 IP"),
                  t("执行用户"),
                  t("结束时间 / 原因"),
                ].map((label) => (
                  <th key={label} className="whitespace-nowrap p-2 font-medium">
                    {label}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {status.data?.sessions.map((item) => (
                <tr key={item.id} className="border-t">
                  <td className="whitespace-nowrap p-2">
                    {formatDateTime(item.started_at ?? item.created_at)}
                  </td>
                  <td className="p-2">
                    {item.actor}
                    <br />
                    {item.remote_addr}
                  </td>
                  <td className="p-2">{item.user}</td>
                  <td className="p-2">
                    {item.ended_at
                      ? `${formatDateTime(item.ended_at)} · ${reasonLabel(item.reason)}`
                      : item.status === "running"
                        ? t("已连接")
                        : t("正在连接")}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {status.data?.sessions.length === 0 ? (
            <p className="p-3 text-sm text-muted-foreground">
              {t("暂无终端会话")}
            </p>
          ) : null}
        </div>
      </div>
      <ReauthenticationDialog
        open={reauth}
        onOpenChange={setReauth}
        onSuccess={() => void connect()}
        description={t("连接终端需要当前密码和现有双因素凭据")}
      />
    </div>
  );
}
