# Preconfigured subagent types

Each `<name>.md` here is a subagent type for Pi Pocket's `subagent` tool, available in every session. A session's
own folder can add or override types in `.pi/agents/` (or `.agents/agents/`), which win over these. Files starting
with `_` are skipped, and unknown frontmatter keys are ignored, so a file written for `pi`'s subagents works here too.

```markdown
---
model: provider/modelId          # an available model; the type's model wins over the caller's
thinking: low                    # off | minimal | low | medium | high | xhigh | max (optional; use a level the model supports)
tools: read, bash, web_search    # what it may use; the caller's list applies where this is unset (optional)
description: One line, shown to the agent when it picks a type (optional)
---
The markdown body is the subagent's brief: it is prepended to the first message of every spawn of this type.
```

`enabled: false` turns a type off (also one defined in a broader folder). Edit these files to change models,
tools, or instructions — no restart needed: the next spawn sees the change.

## The shipped set, and the workflow they are meant for

| Type | Model | What it is for |
| --- | --- | --- |
| `architect` | `opencode-go/kimi-k3` | Requirements, design, risks, acceptance criteria, an ordered plan. Writes nothing. |
| `coder` | `opencode-go/deepseek-v4.1-flash` | Implements the plan, writes tests, runs them, fixes failures. |
| `reviewer` | `opencode-go/glm-5.3` | Verifies the change against the requirements. Writes nothing. |

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
- `thinking` must be a level the model offers; the model picker lists them. A level a model lacks becomes the
  nearest it has. The `opencode-go` models here offer `low`, `high`, and `max` — `medium` is not among them — which
  is why the types above use `max` and `high`.
