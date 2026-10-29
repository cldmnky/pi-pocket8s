// The three tools the image ships, called for real against the Pi Durable packages in the application: no provider
// credentials, and no network but a loopback server this file starts. It needs Pi's packages where the module lives,
// so it skips in a checkout without them; the Containerfile runs it against the copy it installed.
//
// What it pins is the extension API level: 1.4 tool results carry their parts in `output`, and Pi Pocket's `content`
// shim covers drop-in modules only. A built-in that still answered `content` would give the model nothing at all.
import test from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { mkdtempSync, rmSync } from "node:fs";
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

test("web_search without credentials or a pinned model answers a nonempty output error", { skip: createWebSearch === undefined && unavailable }, async () => {
    const tool = tools().get("web_search");

    assert.ok(tool !== undefined, "the web_search tool must be installed");

    const result = await tool.execute({ query: "anything" }, api, {});

    assert.equal(result.isError, true, "no model can search: the call failed");
    assert.match(outputOf(result), /web search/i, "the error names what failed");
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
    await new Promise((resolve) => {
        server.close(resolve);
    });
});
