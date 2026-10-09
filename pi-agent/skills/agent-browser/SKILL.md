---
name: agent-browser
description: "Drive real web pages with the agent-browser CLI: navigate, snapshot, click, fill forms, extract text, screenshot, keep login state, auth vault. Use when a task needs a rendered browser page or visual verification."
---

# agent-browser

The `agent-browser` CLI is installed and preconfigured in this workspace: it
drives the bundled headless Chromium over CDP. Never run
`agent-browser install` (it would download a second browser) and never pass
`--headed` (there is no display).

Work in a named session per task so parallel agents do not share one browser:

```sh
export AGENT_BROWSER_SESSION="$(agent-browser session id --scope worktree --prefix task)"
```

Core loop: `agent-browser open <url>`, then `agent-browser snapshot -i` and
act on the `@eN` refs; re-snapshot after any page change.
`agent-browser close` when done.

Load the version-matched usage guide when you need more than the loop above:

```sh
agent-browser skills get core          # workflows and troubleshooting
agent-browser skills get core --full   # plus the full command reference
```

Daemon state, profiles, and the auth vault persist in the workspace home.
Prefer the auth vault (`auth save` + `auth login`) for logins so credentials
never pass through prompts; never paste secrets into chat or snapshots.
