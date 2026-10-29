# Preconfigured subagent types

Each `<name>.md` here is a subagent type for Pi Pocket's `subagent` tool, available in every session. A session's
own folder can add or override types in `.pi/agents/` (unless the project is distrusted) or `.agents/agents/` (only
when it is trusted); both win over these. The decision is Pi's, shared with the CLI and the skills (`trust.json` in
Pi's folder, **Menu → Project trust**). Files starting with `_` are skipped, and unknown frontmatter keys are ignored.

```markdown
---
model: provider/modelId
thinking: low
tools: read, bash, web_search
description: One line, shown to the agent when it picks a type
---
The markdown body is the subagent's brief: it is prepended to the first message of every spawn of this type.
```

`model` wins over the caller's when the owner's allow-list has it and it can think at the session's level or lower.
One that is allowed but cannot think that low falls back to the session's model — but only when the session's model
is allowed too, because with a nonempty allow-list only listed models may run at all. A model the list does not have,
or an unknown one, is refused by the built-in. Use `provider/modelId`: the fallback check needs the full spec, so a
bare model id is left to the built-in. `thinking` and `tools` apply where the caller set none; the built-in lowers a
level the model lacks to the nearest it supports below it and refuses a tool the caller does not have. Values are
read as plain text: no inline `#` comments, quoted values, or YAML arrays.

`enabled: false` turns a type off (also one defined in a broader folder). Edit these files to change models,
tools, or instructions — no restart needed: the next spawn sees the change.

## The shipped set, and the workflow they are meant for

| Type | Model | What it is for |
| --- | --- | --- |
| `architect` | `opencode-go/kimi-k3` | Requirements, design, risks, acceptance criteria, an ordered plan. Writes nothing. |
| `coder` | `opencode-go/deepseek-v4.1-flash` | Implements the plan, writes tests, runs them, fixes failures. |
| `reviewer` | `opencode-go/glm-5.3` | Verifies the change against the requirements. Writes nothing. |

What a spawn may run on is the owner's allow-list in **Menu → Providers**, and Pi Pocket 0.20 and later read it
strictly: an empty list means the session's own model only, a nonempty one means only the models it lists — the
session's own included, so a spawn that names no model is refused while that model is off the list. A type's model
the list does not have is refused, never quietly replaced. When the type's model *and* the session's are both allowed
and the type's cannot think at the session's level or lower, the type falls back to the session's model instead of
failing (the started message names the model actually used); this is why `architect` on `kimi-k3`, which offers only
`max`, also works from a lower session whose model is allowed. The image's entrypoint allows the shipped types' models
once per workspace (through the app's API, recorded in `~/.pi-pocket/.subagent-models-seeded`), so they work without
setup. A model the owner stops allowing there stays stopped, across restarts too; delete that marker and restart to
seed the shipped models again.

For non-trivial development work: **architect → coder → reviewer**, then send the findings back to the coder and
have the reviewer verify (at most two review/fix cycles, then report what is unresolved). Keep the plan, the merges,
and the final checks in the main conversation; use a git worktree per feature; never let two agents edit the same
file at once. Trivial changes skip all of this.

## Differences from other subagent setups

- There is no maximum-turn setting: Pi Pocket's `subagent` tool has none, so a type cannot cap its own turns.
- There is no concurrency limit and no depth setting: a subagent cannot spawn subagents at all (the `subagent` tool
  is removed from its tools), so depth is always 1.
- There is no per-type worktree isolation. Subagents work in the session's folder, so start the session in a
  worktree (New session → worktree) and every subagent it spawns works there too.
- "Read-only" is enforced by the tool list: `architect` and `reviewer` have no `write` or `edit`. `bash` can still
  change files, so it is a prompt-level rule unless Lancet Guard is on, which checks bash calls.
- `thinking` must be a level the model offers; the model picker lists them. Pi Pocket 0.25 no longer caps the level
  at the session's: a level a model lacks becomes the nearest it supports below it, else its lowest. The shipped
  models differ: `kimi-k3` offers only `max`; `deepseek-v4.1-flash` and `glm-5.3` offer `low`, `high`, and `max` (no
  `medium`). The fallback above is why `architect` still works from a lower session.
