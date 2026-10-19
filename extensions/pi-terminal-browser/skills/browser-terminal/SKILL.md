---
name: browser-terminal
description: Drive an explicitly approved browser-hosted TTY terminal with Pi Terminal Browser.
---

# Browser terminal workflow

1. Call `browser_terminal` with `action: "detect"` before interacting.
2. If several approved tabs or frames match, use the returned tab and frame IDs
   explicitly on every later call.
3. Call `focus`, then use `write` in `fast` mode for literal command text.
4. Send Enter and control chords with `press`; do not append control characters
   to ordinary text.
5. After each mutation, call `read` with `delta: true` or `wait_for` with a
   narrow sentinel. Use a screenshot if structured output is unavailable.
6. Never pass a password, private key, bearer token, or API key as literal
   `text`. Use `secretRef`.
7. Use `interrupt` for Ctrl+C. Treat Ctrl+D as potentially session-ending.
8. Do not enable `browser_raw_cdp` unless typed tools cannot perform the task.

For long shell payloads, prefer a quoted heredoc or base64 transport appropriate
to the target shell, but still deliver it using terminal `write` fast mode.
