---
name: ariadne-remote
description: Inspect, diagnose, build, and operate computers connected through the Ariadne MCP server. Use when work targets an Ariadne node or a remote Windows, Linux, or Android machine reached through Ariadne; do not use for ordinary local commands or generic SSH administration.
---

# Ariadne Remote

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
