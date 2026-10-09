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
 * that file does not exist. It is a model call on that provider's account, so it costs tokens.
 */
import { defineExtension, defineTool } from "@earendil-works/pi-durable";
import type { PocketHost } from "/opt/pi-pocket/src/server/host.ts";
import { modelRegistry, searchContext } from "./web-search/model.ts";
import { pickSearchModel } from "./web-search/select.ts";
import { webSearch, WebSearchSchema } from "./web-search/vendor/web_search.ts";

/** A hung provider call must not hold the tool call open forever. */
const SEARCH_TIMEOUT_MS = 180_000;

const webSearchTool = defineTool({
    name: "web_search",
    description:
        "Search the web with the provider's own search tools (Google Gemini, OpenAI/Codex Responses, xAI Grok, Anthropic, DeepSeek, Ollama Cloud, OpenCode Zen/Go). Use it for anything that changed recently, and pass urls to read specific pages along with the search.",
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

export default function createWebSearch(_host: PocketHost) {
    return defineExtension({ name: "pocket-web-search", tools: [webSearchTool] });
}
