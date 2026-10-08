/**
 * Which model a web search runs on when the install has not named one.
 *
 * Kept free of runtime imports so `web-search.test.mjs` can exercise it with plain `node --test`,
 * without Pi or its packages present.
 */
import type { Api, Model } from "@earendil-works/pi-ai";
import { getProviderKind } from "./vendor/providers/config.ts";

/**
 * Providers whose native search API the vendored code calls correctly, most preferred first.
 *
 * Deliberately a provider/api allow-list and not a guess from the api name: an OpenAI-compatible
 * gateway reports an api it does not implement the same way (OpenRouter answers with Anthropic's
 * message shape and OpenAI's completions shape without implementing either provider's search
 * tool), so it is never picked on its own. Set `provider` and `model` in `web-search.json` to use
 * one anyway.
 */
export const PREFERRED: ReadonlyArray<{ provider: string; api?: string; model?: string }> = [
    { provider: "openai-codex", api: "openai-codex-responses" },
    { provider: "opencode-go", api: "openai-responses" },
    { provider: "xai", api: "openai-responses" },
    { provider: "anthropic", api: "anthropic-messages" },
    { provider: "google-generative-ai" },
    { provider: "antigravity" },
    { provider: "deepseek" },
    { provider: "ollama-cloud" },
];

/** Position in `PREFERRED`; -1 when this model is not picked automatically. */
export function rank(model: Model<Api>): number {
    return PREFERRED.findIndex(
        (entry) =>
            entry.provider === model.provider &&
            (entry.api === undefined || entry.api === model.api) &&
            (entry.model === undefined || entry.model === model.id),
    );
}

/**
 * The model a search runs on: the best-ranked available one, by provider order and then id so the
 * choice does not wander between calls.
 */
export function pickSearchModel(available: readonly Model<Api>[]): Model<Api> | undefined {
    return available
        .filter((model) => getProviderKind(model) !== "unsupported" && rank(model) >= 0)
        .sort((a, b) => rank(a) - rank(b) || a.id.localeCompare(b.id))[0];
}
