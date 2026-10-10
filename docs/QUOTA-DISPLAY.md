# Account quota (Agent v0.1.15)

Desktop terminals display a quota panel at the bottom of the left sidebar with a
manual Refresh button. Mobile users can open the same panel through the session
sidebar. Queries require ownership of the session and an online compatible Agent.
Queries only run for locally allowed CLI commands, and share a 15-second cache.

These are **the device's native logged-in accounts**, not necessarily the provider
or account selected inside the displayed terminal. The UI labels this distinction.
Tokens, OAuth credentials and API keys never appear in the quota response or logs.
Queries do not start conversations or invoke a model. Credentials are not refreshed
or rewritten; expired logins require logging in again inside the native CLI.

- **Codex:** official `app-server` initialize + `account/rateLimits/read`. Window
  durations come from the response; zero use is valid. API-key accounts may not
  expose subscription quota. Configured custom CLI arguments are conservatively
  unsupported so quota queries cannot execute prompts or silently select a different
  profile. `CODEX_HOME` is respected through the Agent's configured environment.
- **Claude:** read-only OAuth usage endpoint, using `.credentials.json` from
  `CLAUDE_CONFIG_DIR` or `~/.claude`. This is an undocumented endpoint and can change.
  API-key billing, macOS keychain credentials, and custom gateways are unsupported.
  This does not modify statusline configuration. Native statusline collection is a
  future alternative for keychain-only accounts.
- **OpenCode:** official Go usage endpoint, using `OPENCODE_API_KEY` from Agent env
  or `opencode-go` API credentials in `XDG_DATA_HOME/opencode/auth.json` (default
  `~/.local/share/opencode/auth.json`). Only Go rolling/weekly/monthly windows are
  supported. Zen balance and third-party OAuth/provider accounts are unsupported.

Missing/malformed values, authorization failures and unavailable providers are
shown explicitly, never converted to 100% remaining. Failed refreshes retain
previous browser data with a warning and its original timestamps. No periodic
polling or scraping of browser cookies is performed.

Desktop file browsing opens a right-hand panel in the existing terminal route.
Text/image previews and uploads/downloads retain the terminal attachment. Mobile
continues to use the dedicated file page. Preview/transfer limits and Agent
workspace authorization are unchanged.

Agent v0.1.21 queries only the CLI launched in the requested session. Codex, Claude and OpenCode use independent 15-second caches; other terminals do not show quota. The browser also filters older Agent responses, so previously cached providers never appear in another CLI session. Windows executable paths and npm shims are recognized.
