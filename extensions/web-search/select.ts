/**
 * Which model a web search runs on when the install has not named one.
 *
 * Kept free of runtime imports so `web-search.test.mjs` can exercise it with plain `node --test`,
 * without Pi or its packages present.
 */
import type { Api, Model } from "@earendil-works/pi-ai";
import { getProviderKind } from "./vendor/providers/config.ts";

/** A model the picker and the config tool consider: whatever the model registry hands out. */
export type SearchCandidate = Model<Api>;

/**
 * Providers whose native search API this build has verified, most preferred first.
 *
 * Deliberately a provider/api allow-list, not a guess from the api name: a gateway serves many
 * providers behind one endpoint under whichever api shape it advertises, and whether it implements
 * that provider's search tool is per-gateway and unverified here. Naming one in `web-search.json`
 * is allowed — OpenRouter's Anthropic-shaped models were verified to run the search tool — it is
 * just never chosen on its own. The result's `grounded` detail says whether a search happened.
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
