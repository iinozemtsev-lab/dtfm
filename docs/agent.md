# `lmdtfy` — Agent Operational Guide

`lmdtfy` (*Let Me Do That For You*) dispatches commands from your agent sandbox to the human operator's `dtfm` terminal for interactive `[y]` approval. Passwords, SSH keys, and cloud tokens stay on the operator's machine.

## 1. Install & Verify Connectivity

Install `lmdtfy` into `~/.local/bin` (or `/usr/local/bin` as root) if missing:

```sh
command -v lmdtfy >/dev/null || \
  curl -fsSL https://raw.githubusercontent.com/iinozemtsev-lab/dtfm/main/scripts/install.sh | sh -s -- lmdtfy
```

Ensure `~/.local/bin` is in `PATH`, then verify an active `dtfm` listener:

- **Local / SSH Unix socket**: `lmdtfy ping` (or `--socket <path>` / `LMDTFY_SOCKET`)
- **E2EE Magic Link (relay)**:
  ```sh
  export LMDTFY_RELAY="<magic-link>"
  lmdtfy ping
  ```
  Or pass `--relay "<magic-link>"` per command (add `--relay-url wss://...` / `LMDTFY_RELAY_URL` only for bare `<session>#<key>` tokens on a self-hosted relay).

## 2. Execution Rules

```sh
lmdtfy exec --why "<concise reason>" [--cwd <dir>] [--timeout <sec>] -- <cmd> [args...]
```

- **Direct `execvp` (no implicit shell)**: Arguments are executed directly. Wrap pipes (`|`), `&&`, redirects, or heredocs in `sh -c '...'` (or `bash -c '...'`).
- **Keep commands reviewable**: State the concrete side effect in `--why`. Prefer declarative writes (`sudo tee /path <<'EOF'`) and batched read-only checks over noisy loops. Multiline commands render with numbered gutters and **cannot** be inline-edited (`[e]`) by the operator—only `[y]` Run or `[n]` Reject.
- **Child `stdin` is `/dev/null`**: Pass non-interactive flags (`-y`, `--force`, `DEBIAN_FRONTEND=noninteractive`). Authenticators (`sudo`, `ssh`, `gpg`) read operator passwords or Touch ID directly from `/dev/tty`.
- **Check `stderr` for edits**: If the operator uses `[e]` on a single-line command, `lmdtfy` prints `[lmdtfy note: command was modified by user: ...]` on `stderr`.
- **Timeouts & directory**: Use `--cwd <dir>` to set the working directory and `--timeout <sec>` (default `300`, max `86400`) when commands need longer.

## 3. Exit Codes

| Code | Meaning | Required Agent Action |
| :---: | :--- | :--- |
| `0` | Approved and exited `0` | Proceed. (Other non-zero codes without a gateway banner are the child command's exit code.) |
| `2` | `dtfm` listener inactive | **Do not retry in a loop.** Ask the operator to start `dtfm`. |
| `124` | Approval or execution timed out | Report timeout; ask operator before retrying or raising `--timeout`. |
| `126` | **Human rejected** or security denial | **STOP and ask the operator. NEVER auto-retry, rephrase, or wrap a vetoed command.** |
