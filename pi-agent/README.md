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
the entrypoint instead merges the one override in, leaving every other key
alone and forcing this one — `opencode-go/kimi-k3`'s
`compat.supportsStrictMode` — to `false` even when the file says `true`. A
workspace that turned it back on would send exactly the requests the gateway
rejects, which is the failure this file exists to prevent. A file that does not
parse, or a pod without `jq`, is reported in the log and left untouched. Remove
the override from your copy once upstream's catalog stops advertising strict
mode for this model.

## MCP servers

Two more files are image-resident and root-owned; the entrypoint seeds one of
them, and the other is read straight from the image:

- `mcp.json` → `~/.pi/agent/mcp.json`, copy-if-missing like the files above. It
declares one server, `kubernetes`, which runs `/usr/local/bin/kubernetes-mcp-server`
with `--config /usr/share/pi-agent/mcp/kubernetes-read-only.toml`, and describes it
as read-only cluster access for the pod's service account. **It starts nothing by
itself**: MCP support comes from the gallery's **mcp** extension, which this image does
not ship, so the entry is inert until the owner installs it and switches `kubernetes`
on. Edit or delete your copy freely — it is your file once seeded.
- `mcp/kubernetes-read-only.toml`, used from `/usr/share/pi-agent/` and never copied
into the workspace. `log_level = 2`, `read_only = true`, `toolsets = ["core"]`, the
kubeconfig the entrypoint writes for the pod, and one `[[denied_resources]]` entry for
the core `v1` `Secret` kind. Keys are checked against the server's reference at the
pinned tag (`docs/configuration.md` of `v0.1.1`): an unknown key makes the server exit
at startup, which is why the image build asserts this file's shape.

Both are asserted at image build time, and the server itself is started during the
build to check its tool list: every tool it registers must be annotated `readOnlyHint`,
and no write tool may be callable by name (`images/mcp-server-runtime.test.mjs`).
