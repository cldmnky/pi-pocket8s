# Pi agent configuration shipped in the image

These files are copied to `/usr/share/pi-agent/` in the image, and the
entrypoint seeds them into the workspace home on boot:

- `skills/agent-browser/SKILL.md` → `~/.pi/agent/skills/agent-browser/SKILL.md`
  (Pi loads it on matching tasks, or forced via `/skill:agent-browser …`).
- `prompts/agent-browser.md` → `~/.pi/agent/prompts/agent-browser.md`
  (becomes the `/agent-browser …` slash command in every session).
- `models.json` → `~/.pi/agent/models.json`. One entry, and it is a fix rather
  than a preference: `opencode-go` advertises `kimi-k3` as supporting OpenAI
  strict-mode tools, but the gateway's upstream rejects Pi's tool schemas in
  strict mode (`'additionalProperties' is required to be supplied and to be
  false`), so every tool-using request to that model fails with HTTP 400 —
  which is what made a `kimi-k3` subagent never answer. The override turns
  `compat.supportsStrictMode` off for that one model, verified with a real tool
  call. Pi reads `models.json` once, at startup.

Seeding is copy-if-missing: edits inside the pod survive restarts, and
deleting a file re-seeds the shipped copy on the next pod start. That is also
how updates reach an existing volume — remove the old file (or directory) and
restart the pod.

`models.json` is the exception: a workspace that already has one keeps it, and
the entrypoint instead merges the one override in (only where it is missing,
leaving other keys alone) so a workspace with its own models does not lose the
fix. A file that does not parse, or a pod without `jq`, is reported in the log
and left untouched. Remove the override from your copy once upstream's catalog
stops advertising strict mode for this model.
