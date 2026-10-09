# web-search

The agent's `web_search` tool, shipped inside the pi-pocket image as a Pi Pocket
**built-in extension**: `images/Containerfile` installs this directory as
`src/server/extensions/web-search.ts` plus `src/server/extensions/web-search/`
in the application, where Pi Pocket's extension loader finds it and loads it by
default. It needs no PVC file and no enabling, and the owner can turn it off (or
back on) in Menu → Extensions like any other module.

A search runs through the **model provider's own search API**, not a scraper:
Google Gemini grounding (and URL Context), OpenAI and Codex Responses, xAI Grok,
Anthropic, DeepSeek, Ollama Cloud, OpenCode Zen/Go.

| File | What it is |
| --- | --- |
| `web-search.ts` | The module the loader imports: one `web_search` tool. |
| `web-search/select.ts` | Which model a search runs on when the install has not named one. |
| `web-search/model.ts` | Pi's model runtime, and the context shim the vendored code expects. |
| `web-search/vendor/` | pi-web-search 1.7.0, unmodified — see `NOTICE.md`. |
| `web-search.test.mjs` | Unit test for the picker. `make test` runs it. |

Two layers exist because pi-web-search is a **Pi** extension (`pi.registerTool`,
pi-tui rendering) and Pi Pocket loads **Pi Durable** extensions
(`defineExtension`/`defineTool`). `pi install npm:pi-web-search` cannot reach the
agent inside this image: nothing in Pi Pocket reads Pi's `settings.json`
packages. The port replaces only the entry point; the provider code is reused
unmodified, so it can be diffed against a later release.

## Choosing the search model

The tool does it: **`web_search_config`**, next to `web_search` in a session.

| Call | Effect |
| --- | --- |
| `{action:"show"}` (default) | what searches now, and whether the pin still resolves |
| `{action:"list"}` | the verified search models available on this install |
| `{action:"set", model:"gpt-5.6-sol"}` | pin it; `provider` only needed when several providers offer that model |
| `{action:"clear"}` | back to automatic |

So a person asks in plain words — "use Gemini for web searches", "which model is
searching?" — and the agent calls the tool. Setting one is idempotent, validated
against the models the install actually has, and reported back with what it will
now use; a stale or unusable pin is reported rather than left to fail quietly.

The same thing by hand, in `~/.pi/agent/web-search.json` — the file upstream's
format already uses, read on every search, so no restart:

```json
{ "provider": "opencode-go", "model": "muse-spark-1.3-contributor" }
```

`PI_WEB_SEARCH_CONFIG` moves that file elsewhere. Without a pin, the tool picks
the highest-ranked *available* model from a verified allow-list (`select.ts`). A
gateway is never picked on its own: it serves many providers behind one endpoint
under whichever api shape it advertises, and whether it implements that
provider's search tool is per-gateway. Naming one explicitly works — OpenRouter's
Anthropic-shaped models were verified to run the search tool — it is just not a
safe default.

### No picker UI, on purpose

A Pi Pocket extension can only carry tools, prompt sections, hooks, wraps and
tasks; it cannot register a command, a sheet or a settings panel. So the choice
is made through the conversation (by a tool), or by the operator in the portal —
not by a dropdown in a session.

## Which engine answers, and whether it searched

There is no separate engine switch: the **provider of the chosen model runs the
search**, in its own way — Google Search grounding for Gemini, the OpenAI
Responses `web_search` tool for OpenAI/Codex and for OpenCode Zen/Go's Responses
models, xAI's search for Grok, Anthropic's `web_search` tool (and DeepSeek's
Anthropic-compatible one), Ollama Cloud's `/api/web_search`. Choosing
`google-generative-ai` vs `openai-codex` is choosing the search backend.

Every result carries details that say what answered:

| Detail | Meaning |
| --- | --- |
| `model` | the model that ran the search |
| `provider` | the dialect used (`google`, `openai`, `xai`, `anthropic`, `deepseek`, `ollama`) |
| `grounded` | whether search results actually came back |
| `results` | how many were found |

Whether to search is the **model's** decision: asked something vague
("anything"), a search-capable model answered from memory with
`grounded: false` rather than searching. The tool then appends a short line to
the answer so that this — and a model whose provider has no search tool — is not
mistaken for a search that worked.

Costs, from the operator's account: a search is a model call on the search
provider, and the result text then goes to the model the conversation is using.
Each search is bounded at 180 seconds.

## Refreshing the vendored code

```bash
npm pack pi-web-search@<version>          # or clone the repository
tar xzf pi-web-search-*.tgz
cp -a package/src/. extensions/web-search/vendor/
# drop the package's own index.ts again: it is Pi's entry point, not Pi Pocket's
```

Then re-check `NOTICE.md`, re-run `make test`, and build the image
(`make image`) — the Containerfile fails the build if the module no longer loads
or stops installing its tool.

## Verifying by hand

Inside a running image:

```bash
node --input-type=module -e '
  const { default: create } = await import("/opt/pi-pocket/src/server/extensions/web-search.ts");
  const extension = create({ notice() {} });
  console.log(extension.name, extension.tools.map((tool) => tool.name).join(","));
'
```

A real search is a billable model call; run one from a conversation, or check
Menu → Extensions for the module and its error state.

## Known rough edge

Inline citation markers can land mid-word in the answer text (`th[1]e`). That is
the vendored formatter placing provider citation offsets, not the adapter; the
source list at the end of the result is unaffected.
