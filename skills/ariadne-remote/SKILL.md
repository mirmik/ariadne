---
name: ariadne-remote
description: Inspect and operate computers through the Ariadne MCP server only when the user explicitly requests Ariadne or the target is already established as an Ariadne node. Do not use it for a generic remote machine, an SSH destination, or merely because the Ariadne MCP server is installed; prefer the user's existing SSH path.
---

# Ariadne Remote

## Routing boundary

Choose Ariadne only after the request or established context has selected it as
the transport. The mere presence of this skill or the Ariadne MCP server, a
remote Windows/Linux/Android target, or a need for remote access is not a reason
to probe Ariadne.

- If the user provides an SSH host, SSH config alias, SSH command, or otherwise
  has working direct SSH access, use SSH. Do not call `ariadne_nodes` first and
  do not replace SSH with Ariadne unless the user asks for that change.
- Do not call `ariadne_nodes` just to discover whether an unrelated remote
  machine might also be reachable through Ariadne.
- If Ariadne was selected and `ariadne_nodes` returns no suitable nodes, report
  that Ariadne currently has no matching node. Do not keep retrying or treat
  Ariadne as a fallback transport for an SSH target.

Use the Ariadne MCP tools directly. Do not invoke the `ari` CLI unless the user is explicitly diagnosing the MCP integration itself.

1. Call `ariadne_nodes` before remote work. Select a claimed alias when available; otherwise use the exact node ID. Do not claim an untrusted alias without confirming which node the user intends.
2. Prefer `ariadne_exec` with `command` for non-interactive work, and use `cwd` instead of a shell `cd` when practical. The default shell is PowerShell on Windows and a POSIX shell on Linux and Android, so normal pipelines, redirects, expansion, and command chaining work directly.
3. Use `shell` only to override automatic selection with `posix`, `powershell`, or `cmd`. Use `argv` instead of `command` only when exact no-shell argument boundaries matter; never provide both.
4. Use `ariadne_job_start` instead of a long `ariadne_exec` when work should continue without holding the agent call or across a relay disconnect. Use `ariadne_job_status` for cheap polling and `ariadne_job_read` with the returned independent stdout/stderr offsets until both EOF flags are true. Cancel jobs that are no longer needed and remove completed jobs when their retained output is no longer useful. Jobs require `background-jobs.v1`; they survive network reconnects but not connector process restarts.
5. Use `ariadne_file_upload` and `ariadne_file_download` for file copies. `local_path` is on the MCP host and `remote_path` is on the selected node; file bytes are streamed outside model context. Leave `overwrite` false unless replacement is intended. Do not encode files into exec commands.
6. Read `platform`, `architecture`, and `capabilities` from `ariadne_nodes` before choosing paths or commands. Preserve Windows path syntax on Windows nodes; file tools require `file-transfer.v1`.
7. Check returned shell, hashes, sizes, job state, output offsets, and truncation flags. Treat `ok: false`, a nonzero `exit_code`, `timed_out`, `canceled`, truncation, and transport-level tool errors as distinct outcomes. Report the remote stderr or concrete operation error.
8. Remote operations have the permissions and environment of the connector user. Apply the user's authorization boundaries exactly; inspect first and request authority before materially broader, destructive, or overwriting changes.
9. Ariadne MCP is non-interactive. If the task genuinely requires interactive terminal control, stdin to a background process, durable execution across connector restarts, or another missing primitive, stop and identify the Ariadne capability to add instead of working around it through an unrelated access path.

Use `ariadne_claim` only with an exact `node_id` returned by `ariadne_nodes` and a user-approved alias.
Use `ariadne_revoke` only with an exact `node_id` returned by `ariadne_nodes` and explicit user authorization for that identity: revoke disconnects it, permanently rejects the same key, and releases its alias for reuse.
