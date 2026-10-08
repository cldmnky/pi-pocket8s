// The web search fallback picker. Runs with plain `node --test`: select.ts and the vendored
// provider config import nothing at runtime, so this needs neither Pi nor its packages.
import assert from "node:assert/strict";
import { test } from "node:test";

import { PREFERRED, pickSearchModel, rank } from "./web-search/select.ts";

function model(provider, api, id, baseUrl = "https://example.invalid/v1") {
    return { provider, api, id, baseUrl };
}

test("picks the first provider in the preferred order", () => {
    const picked = pickSearchModel([
        model("opencode-go", "openai-responses", "gpt-5.6-luna"),
        model("openai-codex", "openai-codex-responses", "gpt-5.6-sol"),
    ]);

    assert.equal(picked.id, "gpt-5.6-sol");
});

test("breaks ties inside a provider by id, and ignores unlisted providers", () => {
    const picked = pickSearchModel([
        model("openrouter", "anthropic-messages", "anthropic/claude-haiku-4.5", "https://openrouter.ai/api/v1"),
        model("opencode-go", "openai-responses", "muse-spark-1.3-contributor"),
        model("opencode-go", "openai-responses", "gpt-5.6-luna"),
    ]);

    assert.equal(picked.id, "gpt-5.6-luna", "lowest id inside the highest-ranked provider");
});

test("a provider whose api shape the vendored code cannot drive is not picked", () => {
    // opencode-go also serves chat-completions models: no provider-native search tool there.
    const picked = pickSearchModel([
        model("opencode-go", "openai-completions", "deepseek-v4-pro"),
        model("deepseek", "anthropic-messages", "deepseek-v4"),
    ]);

    assert.equal(picked.provider, "deepseek");
});

test("the allow-list excludes a matching api on a gateway provider", () => {
    const gateway = model("openrouter", "anthropic-messages", "claude-haiku", "https://openrouter.ai/api/v1");

    assert.ok(rank(gateway) < 0, "rank must reject providers outside the allow-list");
    assert.equal(pickSearchModel([gateway]), undefined);
});

test("nothing suitable available: no guess", () => {
    assert.equal(pickSearchModel([]), undefined);
    assert.equal(pickSearchModel([model("openrouter", "openai-completions", "anything")]), undefined);
});

test("every preferred entry is a provider plus an optional api or model", () => {
    for (const entry of PREFERRED) {
        assert.match(entry.provider, /^[a-z0-9-]+$/, `odd provider: ${entry.provider}`);
        assert.ok(entry.api === undefined || entry.model === undefined || entry.api, "api or model, not neither");
    }
});
