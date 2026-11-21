// The three tools the image ships, called for real against the Pi Durable packages in the application: no provider
// credentials, and no network but a loopback server this file starts. It needs Pi's packages where the module lives,
// so it skips in a checkout without them; the Containerfile runs it against the copy it installed.
//
// What it pins is the extension API level: 1.4 tool results carry their parts in `output`, and Pi Pocket's `content`
// shim covers drop-in modules only. A built-in that still answered `content` would give the model nothing at all.
//
// Octen's endpoint is fixed in production, so its half of the file mocks the destination in this process: the global
// `fetch` answers Octen's address from a fixture and passes every other address (the loopback server) through.
import test from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const unavailable = "Pi runtime not installed";

// Before the module's first tool call: no auth.json, no web-search.json, nothing of this machine's install. Pi's own
// agent directory is the one its environment names, so both are pointed at a fresh temporary folder.
const home = mkdtempSync(join(tmpdir(), "web-search-runtime-"));
const agentDir = join(home, ".pi", "agent");
process.env.HOME = home;
process.env.PI_CODING_AGENT_DIR = agentDir;
delete process.env.PI_WEB_SEARCH_CONFIG;

const moduleUrl = process.env.WEB_SEARCH_MODULE ?? new URL("./web-search.ts", import.meta.url).href;

let createWebSearch;
try {
    ({ default: createWebSearch } = await import(moduleUrl));
} catch {
    // The repository test runner has no Pi packages; the image build does.
}

process.on("exit", () => {
    rmSync(home, { recursive: true, force: true });
    rmSync(pinDir, { recursive: true, force: true });
});

/** The tool's answer, with the checks every tool's answer gets: output parts, text, and no `content` at all. */
function outputOf(result) {
    assert.equal(result.content, undefined, "an API 2 result carries no content");
    assert.ok(Array.isArray(result.output) && result.output.length > 0, "output must be a nonempty array");

    for (const part of result.output) {
        assert.equal(part.type, "text", "every part is text");
    }

    const text = result.output.map((part) => part.text).join("");

    assert.ok(text.trim() !== "", "the output text must not be empty");

    return text;
}

function tools() {
    const extension = createWebSearch({ notice() {}, agentDir });

    return new Map((extension.tools ?? []).map((tool) => [tool.name, tool]));
}

const api = { conversationId: "runtime-test", callId: "call-1", output() {} };

// The Octen half: a made-up key (never a real credential), a pin file per test, and a mocked endpoint.
const OCTEN_KEY = "runtime-test-key-not-a-credential";
const OCTEN_ENDPOINT = "https://api.octen.ai/search";
const pinDir = mkdtempSync(join(tmpdir(), "web-search-pin-"));
const pinFile = join(pinDir, "web-search.json");
let octenRequests = 0;
let octenRequest;
let octenReply = () => jsonResponse({ code: 0, data: { results: [] } });

function jsonResponse(value, status = 200, headers = {}) {
    return new Response(JSON.stringify(value), {
        status,
        headers: { "content-type": "application/json", ...headers },
    });
}

function pin(body) {
    writeFileSync(pinFile, body === undefined ? "" : JSON.stringify(body));
    process.env.PI_WEB_SEARCH_CONFIG = pinFile;
}

/** Set environment variables for one test, restoring them afterwards. */
async function withEnv(values, body) {
    const before = new Map(Object.keys(values).map((name) => [name, process.env[name]]));

    for (const [name, value] of Object.entries(values)) {
        if (value === undefined) delete process.env[name];
        else process.env[name] = value;
    }

    try {
        return await body();
    } finally {
        for (const [name, value] of before) {
            if (value === undefined) delete process.env[name];
            else process.env[name] = value;
        }
        delete process.env.PI_WEB_SEARCH_CONFIG;
    }
}

// A loopback server for web_fetch: nothing leaves the machine. Started before the tests, so every address below is
// listening, and closed when they are done.
const server = createServer((request, response) => {
    if (request.url === "/text") {
        response.writeHead(200, { "content-type": "text/plain; charset=utf-8" });
        response.end("Hello from the loopback server.");

        return;
    }

    if (request.url === "/binary") {
        response.writeHead(200, { "content-type": "application/octet-stream", "content-length": "4" });
        response.end(Buffer.from([0x89, 0x50, 0x4e, 0x47]));

        return;
    }

    response.writeHead(404, { "content-type": "text/plain" });
    response.end("nothing here");
});

const base = await new Promise((resolve) => {
    server.listen(0, "127.0.0.1", () => {
        resolve(`http://127.0.0.1:${String(server.address().port)}`);
    });
});

// The fixed Octen address, answered here; every other address goes to the real fetch (the loopback server).
const realFetch = globalThis.fetch;
globalThis.fetch = async (input, init) => {
    const url = typeof input === "string" ? input : String(input?.url ?? input);

    if (url === OCTEN_ENDPOINT) {
        octenRequests += 1;
        octenRequest = { url, init };
        return octenReply(init);
    }

    return realFetch(input, init);
};

const OCTEN_HITS = {
    code: 0,
    msg: "ok",
    request_id: "req-runtime-1",
    data: {
        query: "octen search",
        results: [{
            title: "Octen search API",
            url: "https://docs.octen.ai/api-reference/search",
            highlight: "POST /search takes a query and a count.",
            time_published: "2026-02-01T00:00:00Z",
            time_last_crawled: "2026-02-02T00:00:00Z",
        }],
    },
    meta: { usage: { total_tokens: 0 } },
};

test("web_search without credentials or a pinned model answers a nonempty output error", { skip: createWebSearch === undefined && unavailable }, async () => {
    const tool = tools().get("web_search");

    assert.ok(tool !== undefined, "the web_search tool must be installed");

    const result = await tool.execute({ query: "anything" }, api, {});

    assert.equal(result.isError, true, "no model can search: the call failed");
    assert.match(outputOf(result), /web search/i, "the error names what failed");
});

test("web_search with octen/search pinned answers with the links and highlights", { skip: createWebSearch === undefined && unavailable }, async () => {
    const tool = tools().get("web_search");

    await withEnv({ OCTEN_API_KEY: OCTEN_KEY }, async () => {
        pin({ provider: "octen", model: "search" });
        octenReply = () => jsonResponse(OCTEN_HITS);

        const before = octenRequests;
        const result = await tool.execute({ query: "octen search" }, api, {});
        const text = outputOf(result);

        assert.equal(octenRequests, before + 1, "one request to the fixed address");
        assert.equal(octenRequest.url, OCTEN_ENDPOINT);
        assert.equal(octenRequest.init.method, "POST");
        assert.equal(octenRequest.init.redirect, "manual");
        assert.equal(octenRequest.init.headers.authorization, `Bearer ${OCTEN_KEY}`);
        assert.equal(result.isError, undefined, "a working search is not an error");
        assert.match(text, /https:\/\/docs\.octen\.ai\/api-reference\/search/, "the actual link is in the answer");
        assert.match(text, /POST \/search takes a query and a count\./, "the highlight is in the answer");
        assert.equal(result.details?.provider, "octen");
        assert.equal(result.details?.model, "search");
        assert.equal(result.details?.results, 1);
        assert.equal(result.details?.grounded, true, "results arrived, so grounding is claimed");
        assert.equal(result.details?.requestId, "req-runtime-1");
        assert.doesNotMatch(text, new RegExp(OCTEN_KEY), "the credential is never in the answer");
    });
});

test("web_search with octen/search and no results grounds nothing", { skip: createWebSearch === undefined && unavailable }, async () => {
    const tool = tools().get("web_search");

    await withEnv({ OCTEN_API_KEY: OCTEN_KEY }, async () => {
        pin({ provider: "octen", model: "search" });
        octenReply = () => jsonResponse({ code: 0, msg: "ok", data: { query: "nothing", results: [] } });

        const result = await tool.execute({ query: "nothing" }, api, {});
        const text = outputOf(result);

        assert.match(text, /Octen returned no results for "nothing"\./);
        assert.match(text, /Nothing was grounded/);
        assert.equal(result.details?.results, 0);
        assert.equal(result.details?.grounded, false);
    });
});

test("web_search reports an Octen rejection as a failed call, without the key", { skip: createWebSearch === undefined && unavailable }, async () => {
    const tool = tools().get("web_search");

    await withEnv({ OCTEN_API_KEY: OCTEN_KEY }, async () => {
        pin({ provider: "octen", model: "search" });
        octenReply = () =>
            jsonResponse({ code: 401, msg: `key ${OCTEN_KEY} is invalid: upstream detail` }, 401, { "x-upstream-id": "header-secret" });

        const result = await tool.execute({ query: "octen search" }, api, {});
        const text = outputOf(result);

        assert.equal(result.isError, true);
        assert.match(text, /rejected the API key \(HTTP 401\)/);
        assert.doesNotMatch(text, new RegExp(OCTEN_KEY), "the credential is never in the answer");
        assert.doesNotMatch(text, /upstream detail|header-secret/, "no upstream body or header value is quoted");
        assert.equal(result.details?.grounded, undefined, "a failure claims no grounding");
    });
});

test("web_search with an Octen pin but no key says what is missing", { skip: createWebSearch === undefined && unavailable }, async () => {
    const tool = tools().get("web_search");

    await withEnv({ OCTEN_API_KEY: undefined }, async () => {
        pin({ provider: "octen", model: "search" });

        const before = octenRequests;
        const result = await tool.execute({ query: "octen search" }, api, {});

        assert.equal(result.isError, true);
        assert.match(outputOf(result), /needs OCTEN_API_KEY/);
        assert.equal(octenRequests, before, "nothing was sent without a credential");
    });
});

test("web_search keeps the model path for a pin that names a provider's model", { skip: createWebSearch === undefined && unavailable }, async () => {
    const tool = tools().get("web_search");

    await withEnv({ OCTEN_API_KEY: OCTEN_KEY }, async () => {
        pin({ provider: "openai-codex", model: "gpt-5.6-not-installed" });

        const before = octenRequests;
        const result = await tool.execute({ query: "anything" }, api, {});
        const text = outputOf(result);

        // No such model is configured here, so the vendored path reports it: the point is that the
        // Octen branch did not take over a pin that names a model provider.
        assert.equal(result.isError, true);
        assert.match(text, /web search/i);
        assert.doesNotMatch(text, /Octen|OCTEN_API_KEY/, "an Octen error would mean the wrong branch ran");
        assert.notEqual(result.details?.provider, "octen");
        assert.equal(octenRequests, before, "Octen was not called for a model pin");
    });
});

test("web_search_config lists, sets and shows octen/search without a model registry entry", { skip: createWebSearch === undefined && unavailable }, async () => {
    const tool = tools().get("web_search_config");

    await withEnv({ OCTEN_API_KEY: OCTEN_KEY }, async () => {
        pin(undefined);
        octenReply = () => jsonResponse(OCTEN_HITS);

        const listed = outputOf(await tool.execute({ action: "list" }, api, {}));

        assert.match(listed, /- octen\/search \(direct Octen search API, no model call; OCTEN_API_KEY is set\)/);

        const typed = outputOf(await tool.execute({ action: "set", model: "octen/web" }, api, {}));
        assert.match(typed, /one search operation/);

        const set = outputOf(await tool.execute({ action: "set", provider: "octen", model: "search" }, api, {}));
        assert.match(set, /Octen search API \(octen\/search\)/);
        assert.match(set, /no restart is needed/);
        assert.deepEqual(JSON.parse(readFileSync(pinFile, "utf8")), { provider: "octen", model: "search" });

        const shown = outputOf(await tool.execute({ action: "show" }, api, {}));
        assert.match(shown, /Octen search API \(octen\/search, pinned in /);
        assert.match(shown, /OCTEN_API_KEY is set\./);

        // The pinned choice actually searches, through the tool that reads the same file.
        const result = await tools().get("web_search").execute({ query: "octen search" }, api, {});
        assert.match(outputOf(result), /docs\.octen\.ai/);
    });
});

test("web_search_config does not offer octen/search without its key", { skip: createWebSearch === undefined && unavailable }, async () => {
    const tool = tools().get("web_search_config");

    await withEnv({ OCTEN_API_KEY: undefined }, async () => {
        pin(undefined);

        const listed = outputOf(await tool.execute({ action: "list" }, api, {}));

        assert.doesNotMatch(listed, /- octen\/search/);
        assert.match(listed, /Octen's search API needs no model/, "the unavailable choice is still explained");
    });
});

test("web_search refuses an Octen redirect instead of following it", { skip: createWebSearch === undefined && unavailable }, async () => {
    const tool = tools().get("web_search");

    await withEnv({ OCTEN_API_KEY: OCTEN_KEY }, async () => {
        pin({ provider: "octen", model: "search" });
        octenReply = () => new Response(null, { status: 302, headers: { location: "https://elsewhere.test/search" } });

        const before = octenRequests;
        const result = await tool.execute({ query: "octen search" }, api, {});
        const text = outputOf(result);

        assert.equal(octenRequests, before + 1, "no second request: the redirect was not followed");
        assert.equal(result.isError, true);
        assert.match(text, /redirect \(HTTP 302\), which this tool never follows/);
        assert.doesNotMatch(text, /elsewhere\.test/, "the Location header is not passed on");
    });
});

test("web_search and web_fetch carry the replay policy their cost and risk justify", { skip: createWebSearch === undefined && unavailable }, () => {
    const installed = tools();

    // Both search backends are billable: an interrupted search must not be rerun on recovery.
    assert.equal(installed.get("web_search").replay, "unsafe");
    // A fetch is a plain read of one page, and setting a pin is idempotent.
    assert.equal(installed.get("web_fetch").replay, "safe");
    assert.equal(installed.get("web_search_config").replay, "safe");
});

test("web_fetch returns a text body as output", { skip: createWebSearch === undefined && unavailable }, async () => {
    const tool = tools().get("web_fetch");

    assert.ok(tool !== undefined, "the web_fetch tool must be installed");

    const result = await tool.execute({ url: `${base}/text` }, api, {});

    assert.equal(result.isError, undefined);
    assert.match(outputOf(result), /Hello from the loopback server\./);
    assert.equal(result.details?.status, 200);
});

test("web_fetch returns an error status as output", { skip: createWebSearch === undefined && unavailable }, async () => {
    const tool = tools().get("web_fetch");
    const result = await tool.execute({ url: `${base}/missing` }, api, {});

    assert.equal(result.isError, true);
    assert.match(outputOf(result), /answered HTTP 404/);
});

test("web_fetch reports a binary body instead of returning it", { skip: createWebSearch === undefined && unavailable }, async () => {
    const tool = tools().get("web_fetch");
    const result = await tool.execute({ url: `${base}/binary` }, api, {});

    assert.equal(result.isError, undefined, "a binary body is not a failed fetch");
    assert.match(outputOf(result), /application\/octet-stream/);
    assert.equal(result.details?.binary, true);
});

test("web_search_config answers with output, and nothing to search with yet", { skip: createWebSearch === undefined && unavailable }, async () => {
    const tool = tools().get("web_search_config");

    assert.ok(tool !== undefined, "the web_search_config tool must be installed");

    const text = outputOf(await tool.execute({ action: "list" }, api, {}));

    assert.match(text, /No verified search provider is configured on this install\./);
});

test.after(async () => {
    globalThis.fetch = realFetch;

    await new Promise((resolve) => {
        server.close(resolve);
    });
});
