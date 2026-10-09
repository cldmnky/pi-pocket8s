// The web search fallback picker. Runs with plain `node --test`: select.ts and the vendored
// provider config import nothing at runtime, so this needs neither Pi nor its packages.
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, readdirSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { clearPin, describeWriteFailure, pinPath, pinSource, readPin, writePin } from "./web-search/config.ts";
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

function temporaryDirectory() {
    return mkdtempSync(join(tmpdir(), "web-search-test-"));
}

test("the pin the tools write is the file the provider code reads", () => {
    const dir = temporaryDirectory();
    const path = pinPath(dir);

    assert.equal(path, join(dir, "web-search.json"), "default location is Pi's agent directory");
    assert.deepEqual(readPin(path), { path }, "missing file is not an error");

    writePin(path, { provider: "openai-codex", model: "gpt-5.6-sol" });
    assert.deepEqual(readPin(path).pin, { provider: "openai-codex", model: "gpt-5.6-sol" });

    // Must stay in the shape the vendored reader accepts (provider + model, no extra keys).
    assert.deepEqual(JSON.parse(readFileSync(path, "utf8")), {
        provider: "openai-codex",
        model: "gpt-5.6-sol",
    });

    // A second write replaces the first, and leaves no temporary file behind.
    writePin(path, { provider: "opencode-go", model: "muse-spark-1.3-contributor" });
    assert.deepEqual(readPin(path).pin, { provider: "opencode-go", model: "muse-spark-1.3-contributor" });
    assert.deepEqual(readdirSync(dir), ["web-search.json"], "no temporary file is left behind");

    assert.equal(clearPin(path), true, "a pin was there");
    assert.equal(readPin(path).pin, undefined);
    assert.equal(clearPin(path), false, "clearing twice is not an error");
});

test("the model can also be read as modelId, and a broken file is reported not thrown", () => {
    const dir = temporaryDirectory();
    const path = pinPath(dir);

    writeFileSync(path, JSON.stringify({ provider: "xai", modelId: "grok-4.6" }));
    assert.deepEqual(readPin(path).pin, { provider: "xai", model: "grok-4.6" });

    writeFileSync(path, "{ not json");
    assert.match(readPin(path).error, /invalid JSON/);

    writeFileSync(path, JSON.stringify(["openai-codex", "gpt-5.6-sol"]));
    assert.match(readPin(path).error, /JSON object/);

    writeFileSync(path, JSON.stringify({ provider: "  ", model: "gpt-5.6-sol" }));
    assert.match(readPin(path).error, /non-empty provider and model/);
});

test("PI_WEB_SEARCH_CONFIG moves the file, the way the vendored reader expects", () => {
    const elsewhere = join(temporaryDirectory(), "custom.json");
    const previous = process.env.PI_WEB_SEARCH_CONFIG;
    process.env.PI_WEB_SEARCH_CONFIG = elsewhere;

    try {
        assert.equal(pinPath("/some/agent/dir"), elsewhere);
    } finally {
        if (previous === undefined) delete process.env.PI_WEB_SEARCH_CONFIG;
        else process.env.PI_WEB_SEARCH_CONFIG = previous;
    }
});

test("the pin's owner is decided by where it lives", () => {
    assert.equal(pinSource("/run/pocket-config/web-search.json"), "portal");
    assert.equal(pinSource("/workspace/home/.pi/agent/web-search.json"), "workspace");
    assert.equal(pinSource("/somewhere/else/web-search.json"), "workspace");
});

test("a read-only portal pin says who owns the choice instead of failing blind", () => {
    const portal = "/run/pocket-config/web-search.json";
    const workspace = "/workspace/home/.pi/agent/web-search.json";

    // EROFS is what writing into a mounted Secret gives.
    const denied = Object.assign(new Error("read-only file system"), { code: "EROFS" });

    assert.match(describeWriteFailure(portal, denied), /portal sets the search model.*read-only.*restart/s);
    assert.match(describeWriteFailure(workspace, denied), /Cannot write .*EROFS/);

    const missing = Object.assign(new Error("no such file or directory"), { code: "ENOENT" });
    assert.match(describeWriteFailure(workspace, missing), /Cannot write .*no such file or directory/);
});
