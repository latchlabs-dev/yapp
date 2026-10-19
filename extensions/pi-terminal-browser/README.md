# Pi Terminal Browser

`@earlgrey/pi-terminal-browser` lets Pi control a terminal running in an
existing Chrome profile. It uses a bundled Manifest V3 companion extension and
Chrome DevTools Protocol input, so commands reach `xterm.js`, canvas terminals,
`vim`, `tmux`, and other TTY programs as real browser input.

The package is local-only and fail-closed:

- The bridge binds only to `127.0.0.1`.
- A random bearer token authenticates every extension request.
- The user must approve each tab from the Chrome popup.
- Each Pi session needs a time-bounded `/browser authorize` grant.
- Raw CDP needs a second `/browser raw on` grant and blocks destructive methods.
- User-owned tabs are detached, never closed.

## Install from this checkout

```text
pi install /absolute/path/to/piproxy/extensions/pi-terminal-browser
```

Restart Pi, then run:

```text
/browser onboard
/browser pair
```

Open `chrome://extensions`, enable developer mode, choose **Load unpacked**,
and select the path printed by `/browser onboard`. Pin the extension, open its
popup, paste the pairing token printed by `/browser pair`, and select **Pair**.

On the web-SSH tab, open the popup and select **Approve this tab**. Back in Pi:

```text
/browser authorize 30m
/browser attach your-web-ssh-host
/browser status
```

Pi can now use `browser_terminal`, `browser_input`, and `browser_page`.

## Terminal behavior

`browser_terminal.write` defaults to CDP `Input.insertText`, which inserts a
long literal command in one operation. `browser_terminal.press` uses
`Input.dispatchKeyEvent` for Enter, Tab, arrows, Escape, Backspace, Ctrl+C,
Ctrl+D, and modifier chords.

`browser_terminal.read` tries, in order:

1. an exposed xterm buffer,
2. xterm's accessibility tree,
3. the DOM renderer,
4. passive CDP WebSocket frames.

If none is structured, use `browser_page` with `action: "screenshot"`.
`browser_terminal.detect` returns frame IDs that can be passed to later calls.

## Secrets

Do not put passwords or tokens in a tool's `text` field. `browser_input` and
`browser_terminal.write` accept these local references instead:

- `env:NAME`
- `keychain:SERVICE/ACCOUNT` (macOS Keychain)
- `1password:op://vault/item/field`

The Pi extension resolves the value locally and does not include it in tool
results or details. The paired loopback request still contains the plaintext in
memory long enough to deliver it to Chrome.

## Revoke

`/browser revoke` immediately expires Pi grants, rotates the bridge token, and
disconnects the Chrome extension. Pair again before the next session. Closing
Pi also closes the loopback bridge and detaches debugger sessions.

## Scope

This package is intentionally not a remote browser driver. It trusts the local
Pi process and the explicitly paired extension. The Chrome extension requests
`debugger`, `scripting`, `tabs`, `storage`, `webNavigation`, and `<all_urls>`;
install it only in a profile whose contents Pi is allowed to access.
