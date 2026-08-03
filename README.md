# claude-usage

Monitor Claude Max subscription usage across multiple accounts, and (optionally)
auto-switch the account Claude Code is using before a session hits its limit.

It polls the account-wide OAuth usage endpoint that powers Claude's in-app
`/usage` view, for every account you've added, and presents it two ways:

- a **terminal dashboard** (Bubble Tea TUI), and
- a **macOS menu bar app** — `👾 NN%` in the bar (active account's 5-hour usage),
  with a dropdown showing every account's session (5h) and weekly (7d) windows.

macOS only (the menu bar app and the account swap use the macOS Keychain and
Cocoa).

## How it works

Claude Code stores its active OAuth credential in the macOS Keychain under
`Claude Code-credentials`. This tool reads those credentials to poll usage, and
keeps a credential per account in `~/.config/claude-usage/accounts.json`.

Adding extra accounts does **not** disturb your current Claude Code session:
`claude-usage login` runs a standalone browser OAuth flow and stores the tokens
independently.

## Build & install

Requires Go 1.26+.

```sh
make build            # build ./claude-usage
make install          # install to $(PREFIX), default ~/go/bin
```

## CLI usage

```
claude-usage login          browser OAuth for an account (no Claude Code session change)
claude-usage add            capture the account currently logged into Claude Code
claude-usage list           list configured accounts
claude-usage rm <email>     remove an account
claude-usage menubar        run the macOS menu bar app (👾 NN%)
claude-usage                live dashboard
```

Accounts are identified by the email read from their account profile — no manual
naming. Typical first run:

```sh
claude-usage add            # capture the account Claude Code is currently on
claude-usage login          # sign in to another account in the browser, repeat as needed
claude-usage                # watch the dashboard
```

## Menu bar app

The menu bar app must run from a `.app` bundle (menuet initializes macOS
notification services at startup, which requires a bundle). The Makefile builds
and signs one:

```sh
make run-bundle       # build "Claude Usage.app" and open it
make install-bundle   # copy the bundle into /Applications
```

The title shows `👾 NN%` (active account's 5-hour utilization). The dropdown
lists every account with both windows and their reset countdowns, plus:

- **Alert threshold** — the session-usage % that triggers an alert
  (70/75/80/85/90/95/98).
- **Enable Auto-swap on alert** — see below.
- **Swap to** — manually switch Claude Code to any account, any time.

The active account is marked with a green ● dot; when auto-swap is enabled, a
blue `*` marks the account a swap would pick next.

### Auto-swap

Auto-swap triggers when the active account crosses **either** limit:

- **session (5h)** usage reaches the alert threshold, or
- **weekly (7d)** usage reaches **99%**.

On trigger it rewrites the Claude Code keychain credential to a healthier
account. A running Claude Code session picks up the new token on its next
request — if a session stops on an API limit, typing `continue` resumes it on
the new account.

Target selection:

1. Among the other accounts whose session is below the alert threshold, pick the
   one with the **lowest weekly (7d)** usage.
2. If none qualify, defer until the active session reaches **98%**, then pick the
   lowest-weekly account whose session is below 98%.
3. If no account's session is below 98%, do nothing.

Auto-swap is **off by default** — it silently changes which account subsequent
Claude Code requests bill to. Weekly usage is only used to rank candidates,
never as a swap threshold.


## Configuration

`~/.config/claude-usage/accounts.json` holds accounts and settings:

```json
{
  "accounts": [ ... ],
  "settings": {
    "notifyThresholdPct": 90,
    "autoSwapOnAlert": false
  }
}
```

Both settings are also editable from the menu bar dropdown.

## License

[MIT](LICENSE)
