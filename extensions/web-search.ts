/**
 * Web search through the model provider's own search tools: Google Gemini grounding (and URL
 * Context), OpenAI and Codex Responses, xAI Grok, Anthropic, DeepSeek, Ollama Cloud and OpenCode
 * Zen/Go.
 *
 * Shipped as a Pi Pocket built-in extension: the image installs this file as
 * `src/server/extensions/web-search.ts` in the application, where the extension loader picks it up
 * and loads it by default. The provider code is vendored from the pi-web-search package (MIT) under
 * `web-search/` — see `web-search/NOTICE.md`; this file is the adapter, because Pi Pocket
 * extensions use Pi Durable's tool API rather than Pi's.
 *
 * A search runs on the provider named in `~/.pi/agent/web-search.json`
 * (`{"provider":…,"model":…}`), or on the best available model from the known-good providers when
 * that file does not exist — `web_search_config` lists the choices and sets or clears the file. A
 * search is a model call on that provider's account, so it costs tokens.
 */
import { Type } from "@earendil-works/pi-ai";
import { defineExtension, defineTool } from "@earendil-works/pi-durable";
import type { PocketHost } from "/opt/pi-pocket/src/server/host.ts";
import { clearPin, pinPath, readPin, writePin } from "./web-search/config.ts";
import { modelRegistry, searchContext } from "./web-search/model.ts";
import { pickSearchModel, rank, type SearchCandidate } from "./web-search/select.ts";
import { webSearch, WebSearchSchema } from "./web-search/vendor/web_search.ts";

/** A hung provider call must not hold the tool call open forever. */
const SEARCH_TIMEOUT_MS = 180_000;

/** Keep the list answer to a size a phone screen and a model can both use. */
const LIST_LIMIT = 20;

function webSearchTool() {
    return defineTool({
        name: "web_search",
        description:
            "Search the web with the provider's own search tools (Google Gemini, OpenAI/Codex Responses, xAI Grok, Anthropic, DeepSeek, Ollama Cloud, OpenCode Zen/Go). Use it for anything that changed recently, and pass urls to read specific pages along with the search. Which provider searches is the install's setting: use web_search_config to see or change it, or when someone asks for a particular one.",
        parameters: WebSearchSchema,
        // A search is a read: running it again after an interruption is harmless.
        replay: "safe",
        execute: async (args, api) => {
            const models = await modelRegistry();
            const model = pickSearchModel(models.getAvailable());
            const ctx = searchContext(models, model, String(api.conversationId));

            const result = await webSearch(
                api.callId,
                args,
                AbortSignal.timeout(SEARCH_TIMEOUT_MS),
                (update) => {
                    const text = (update?.content ?? [])
                        .map((part) => (part.type === "text" ? part.text : ""))
                        .join("");
                    if (text) api.output(`${text}\n`);
                },
                ctx,
            );

            const text = result.content
                .map((part) => (part.type === "text" ? part.text : ""))
                .join("\n\n");
            const failed = Boolean((result.details as { error?: unknown } | undefined)?.error);
            const reported = (result.details ?? {}) as Record<string, unknown>;

            // The provider code reports what answered and whether anything was grounded. Pass the
            // primitives on: the app shows them with the call, so "which model searched, and did it
            // search at all" is answerable without reading the provider's response.
            const details: Record<string, string | number | boolean> = {};
            if (typeof reported.model === "string") details.model = reported.model;
            if (typeof reported.providerKind === "string") details.provider = reported.providerKind;
            if (typeof reported.resultCount === "number") details.results = reported.resultCount;
            if (typeof reported.grounded === "boolean") details.grounded = reported.grounded;

            // Whether a search happens is the model's decision: a search-capable model may answer a
            // vague question from memory with no search at all, which looks like a search that worked.
            // Say when nothing came back, and name what answered, so a mis-set web-search.json or a
            // provider without a search tool is visible instead of silent.
            let answer = text || "No result.";
            if (!failed && details.grounded !== true) {
                const searched = details.model === undefined ? "the configured model" : details.model;
                answer += `\n\n(No search results came back from ${searched}: it did not search, or that provider has no web search tool.)`;
            }

            return {
                content: [{ type: "text" as const, text: answer }],
                ...(failed ? { isError: true } : {}),
                ...(Object.keys(details).length > 0 ? { details } : {}),
            };
        },
    });
}

/** Everything the two tools answer with, so "which engine is it using" has one source of truth. */
function configTool(agentDir: string) {
    return defineTool({
        name: "web_search_config",
        description:
            "Show, list, set or clear which model performs web searches. Searches use the model's own provider: Google Gemini grounding, OpenAI/Codex Responses, xAI Grok, Anthropic, DeepSeek, Ollama Cloud or OpenCode Zen/Go. Use action=list to see the choices on this install, action=set with a model to choose one, action=clear to go back to automatic.",
        parameters: Type.Object({
            action: Type.Optional(
                Type.String({
                    description: "show (default) | list | set | clear",
                }),
            ),
            model: Type.Optional(
                Type.String({ description: "model id to search with, for action=set" }),
            ),
            provider: Type.Optional(
                Type.String({
                    description: "provider id, only needed when several providers offer that model",
                }),
            ),
        }),
        // Setting a value is idempotent, and clearing one twice is the same as clearing it once.
        replay: "safe",
        execute: async (args) => {
            const path = pinPath(agentDir);
            const models = await modelRegistry();
            const available = models.getAvailable();
            const automatic = pickSearchModel(available);
            const action = (args.action ?? "show").trim().toLowerCase();

            if (action === "show") {
                const file = readPin(path);

                if (file.error !== undefined) {
                    return text(
                        `Web search cannot use ${path}: ${file.error}. Set a model with web_search_config action=set, or clear the file.\n\nAutomatic choice: ${describeModel(automatic)}.`,
                    );
                }

                if (file.pin === undefined) {
                    return text(
                        `Web search picks its own model: ${describeModel(automatic)}. Nothing is pinned in ${path}. Set one with web_search_config action=set.`,
                    );
                }

                const found = models.find(file.pin.provider, file.pin.model);
                const warning =
                    found === undefined
                        ? `\n\nWarning: ${file.pin.provider}/${file.pin.model} is not configured on this install, so searches fail until it is. Pick another, or configure that provider.`
                        : rank(found) < 0
                          ? `\n\nNote: ${file.pin.provider} is not a provider this build verified for search. If answers come back with no search results, pick one from web_search_config action=list.`
                          : "";

                return text(
                    `Web search uses ${file.pin.provider}/${file.pin.model} (pinned in ${path}).${warning}`,
                );
            }

            if (action === "list") {
                const verified = available
                    .filter((model) => rank(model) >= 0)
                    .sort((a, b) => rank(a) - rank(b) || a.id.localeCompare(b.id))
                    .slice(0, LIST_LIMIT)
                    .map((model) => `- ${model.provider}/${model.id}`);

                const head =
                    verified.length === 0
                        ? "No verified search provider is configured on this install."
                        : `Verified search models on this install:\n${verified.join("\n")}${verified.length === LIST_LIMIT ? "\n(and more)" : ""}`;

                return text(
                    `${head}\n\nAny other configured model can be named explicitly with action=set (provider and model); its provider may not implement a search tool, which the result of a search will say. Automatic choice: ${describeModel(automatic)}.`,
                );
            }

            if (action === "clear") {
                const had = clearPin(path);

                return text(
                    had
                        ? `Cleared ${path}. Web search picks its own model again: ${describeModel(automatic)}.`
                        : `Nothing was pinned in ${path}. Web search picks its own model: ${describeModel(automatic)}.`,
                );
            }

            if (action !== "set") {
                return text(`Unknown action "${action}". Use show, list, set or clear.`);
            }

            if (args.model === undefined || args.model.trim() === "") {
                return text(
                    "action=set needs a model. Use web_search_config action=list to see the choices.",
                );
            }

            const wanted = args.model.trim();
            const provider = args.provider?.trim();
            const matches = available.filter(
                (model) =>
                    (model.id === wanted || `${model.provider}/${model.id}` === wanted) &&
                    (provider === undefined || provider === "" || model.provider === provider),
            );

            if (matches.length === 0) {
                return text(
                    `No configured model matches ${provider ? `${provider}/` : ""}${wanted}. Use web_search_config action=list, or name a provider this install has credentials for.`,
                );
            }

            // Prefer a provider this build verified, then the usual order, then a stable id order.
            const chosen = matches.sort(
                (a, b) =>
                    Number(rank(a) < 0) - Number(rank(b) < 0) || rank(a) - rank(b) || a.id.localeCompare(b.id),
            )[0] as SearchCandidate;

            writePin(path, { provider: chosen.provider, model: chosen.id });

            const others = matches.filter((model) => model !== chosen).map(describeModel);
            const alternative =
                others.length === 0
                    ? ""
                    : `\n\nAlso available as ${others.join(", ")} — set provider to pick one of those.`;
            const unverified =
                rank(chosen) < 0
                    ? `\n\nNote: ${chosen.provider} is not a provider this build verified for search. If a search comes back with no results, pick one from action=list.`
                    : "";

            return text(
                `Web search now uses ${chosen.provider}/${chosen.id}, written to ${path}. The next search uses it; no restart is needed.${unverified}${alternative}`,
            );
        },
    });
}

function describeModel(model: SearchCandidate | undefined): string {
    return model === undefined ? "none available" : `${model.provider}/${model.id}`;
}

function text(body: string) {
    return { content: [{ type: "text" as const, text: body }] };
}

export default function createWebSearch(host: PocketHost) {
    return defineExtension({
        name: "pocket-web-search",
        tools: [webSearchTool(), configTool(host.agentDir)],
    });
}
