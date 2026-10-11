# Edge node terminal

The node detail page's **终端 / Terminal** action opens an interactive shell on that
edge node. Upgrade the controller and edge Agent to a build advertising
`terminal_v1`. An active or draining node must have its terminal connection online.
Older Agents show an upgrade prompt; newer Agents retry quietly once a minute if
an older controller does not implement the endpoint.

The first version supports one session per node, terminal input/output (including
UTF-8 and ANSI sequences), copy/paste, Ctrl+C, window resizing, and manual close.
The dialog displays the node, address, execution user, shell, and latest 50 audit
records. Other authenticated administrator tabs can see and end an existing
session; attaching to it or resuming it is not supported.

## Identity and execution environment

Session creation uses the existing administrator session, CSRF protection, and
five-minute recent-authentication window. When necessary, the UI asks for the
administrator password and TOTP/recovery code using the existing reauthentication
flow. The WebSocket requires the exact login session that created it, a matching
Origin host, and a single successful attach. No bearer credentials are placed in
WebSocket URLs.

The shell runs as the Agent's operating-system user: **root** with the supplied
systemd unit. It inherits the Agent's service sandbox, including `ProtectHome=true`
and `PrivateTmp=true`; `/root`, `/home`, and the host's ordinary `/tmp` may therefore
be unavailable or differ from an SSH session. Working directory and HOME are `/`.
The environment is built explicitly (PATH, TERM, LANG, USER, LOGNAME, HOME), rather
than inheriting Agent configuration or enrollment credentials. `/bin/bash -i` is
used where available, falling back to `/bin/sh -i`. HISTFILE defaults to `/dev/null`.
Escape is forwarded to terminal applications such as vim.

## Connection and lifetime

```text
Browser -- same-origin WSS --> Controller <-- outbound mTLS WSS -- Edge Agent
                                                                  |
                                                                 PTY
                                                                  |
                                                                 Shell
```

The Agent maintains one authenticated outbound WebSocket to
`/api/edge/v1/terminal`. It uses a dedicated HTTP/1.1 transport and the existing
node certificate; the usual HTTP/2 control transport is unchanged. No additional
node listener or SSH credentials are required. Browser-facing reverse proxies
must forward WebSocket Upgrade/Connection headers and preserve the original Host.
The Agent must continue connecting directly to the controller's mTLS listener.
The Vite development proxy includes the terminal WebSocket route.

Fixed first-version limits:

- 30 seconds to attach the browser and receive the Agent's shell-ready response.
- 10 minutes without terminal input. Output, window resizing, and keepalives do
  not extend the idle deadline.
- 60 minutes maximum, even while typing.
- 15-second WebSocket pings with a 10-second pong deadline, and bounded messages,
  queues, and write deadlines.

Both controller and Agent enforce terminal time limits. Closing the dialog,
leaving the node page, losing either connection, ending the shell, or stopping the
Agent ends the session. Login expiration/revocation and node authorization are
rechecked every second by the controller. A controller shutdown closes hijacked
connections explicitly. Reconnection of the Agent never resumes an old shell.
PTY cleanup terminates the foreground group and shell group; on Linux it also
terminates remaining processes in the terminal session, including normal
background jobs. Services deliberately launched outside the terminal session
(for example with systemd) retain their own lifecycle.

## Session audit

SQLite migration 42 adds `terminal_sessions`. Creation, shell-ready, and closure
are committed together with `terminal_created`, `terminal_started`, and
`terminal_closed` entries in the existing audit log. Records include session ID,
node ID, administrator ID, source IP, OS user, shell, creation/start/end times,
and closure reason. Commands, keystrokes, terminal output, and credentials are
not recorded. Audit records survive node deletion and are included in the normal
SQLite backup. Records are retained; the UI returns the most recent 50 per node.

Failure to persist creation or start prevents the interactive session from
continuing. A closure persistence failure is logged by the controller; startup
recovery marks any remaining unfinished records `control_restarted`, with the
recovery timestamp as the end time. Normal closure records the actual observed
closure time. This is session metadata auditing, not terminal recording.

## API and protocol

- `GET /api/nodes/{id}/terminal-sessions`: availability, OS identity, timeout
  limits, and latest 50 records.
- `POST /api/nodes/{id}/terminal-sessions`: `{cols, rows}`; returns a pending record.
- `GET /api/nodes/{id}/terminal-sessions/{sessionID}/connect`: browser WebSocket.
- `DELETE /api/nodes/{id}/terminal-sessions/{sessionID}`: end the session.
- `GET /api/edge/v1/terminal`: node-authenticated Agent WebSocket.

JSON messages carry `type`, optional `session_id`, base64 `data`, and `cols`/`rows`.
Browser messages are limited to `input`, `resize`, and `close`. Agent messages are
`hello`, `ready`, `output`, and `exit`; controller messages to the Agent are `start`,
`input`, `resize`, and `close`. Controller-to-browser closure uses `closed` and a
machine-readable `reason`. Terminal sizes are bounded to 2–500 columns and 2–200
rows, with at most 16 KiB of decoded data per message.

## Verification

Use the project's Go 1.26 toolchain. The terminal tests cover real mTLS, real PTY
command execution, UTF-8, Ctrl+C, resize, process cleanup, session ownership,
CSRF/recent authentication, revocation, timeouts, audit transactions and restart
recovery. Playwright covers reauthentication, terminal input/output, resizing,
manual close, audit rendering, and unsupported-Agent messaging.

```bash
go test -race ./internal/control ./internal/edge ./internal/store -run TestTerminal
npm --prefix frontend run check
npm --prefix frontend run test:e2e -- --grep 'node terminal'
```
