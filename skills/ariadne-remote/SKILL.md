---
name: ariadne-remote
description: Inspect, diagnose, build, and operate computers connected through the Ariadne MCP server. Use when work targets an Ariadne node or a remote Windows, Linux, or Android machine reached through Ariadne; do not use for ordinary local commands or generic SSH administration.
---

# Ariadne Remote

Use the Ariadne MCP tools directly. Do not invoke the `ari` CLI unless the user is explicitly diagnosing the MCP integration itself.

1. Call `ariadne_nodes` before remote work. Select a claimed alias when available; otherwise use the exact node ID. Do not claim an untrusted alias without confirming which node the user intends.
2. Prefer `ariadne_exec` for non-interactive work. Pass the executable and arguments separately in `argv`, and use `cwd` instead of a shell `cd`.
3. `argv` is executed directly. Operators such as `&&`, `|`, redirects, variable expansion, and built-ins are not interpreted. When shell syntax is genuinely needed, invoke the platform shell explicitly, such as `cmd.exe /d /c`, `powershell.exe -NoProfile -Command`, or `/bin/sh -lc`.
4. Read `platform` and `architecture` from `ariadne_nodes` before choosing paths or commands. Preserve Windows path syntax on Windows nodes.
5. Treat `ok: false`, a nonzero `exit_code`, `timed_out`, truncation flags, and transport-level tool errors as distinct outcomes. Report the remote stderr and the concrete failing command.
6. Remote exec has the permissions and environment of the connector user. Apply the user's authorization boundaries exactly; inspect first and request authority before materially broader or destructive changes.
7. Ariadne MCP is non-interactive and returns output when the command completes. If the task genuinely requires interactive terminal control, file transfer, live output, or another missing primitive, stop and identify the Ariadne capability to add instead of working around it through an unrelated access path.

Use `ariadne_claim` only with an exact `node_id` returned by `ariadne_nodes` and a user-approved alias.
