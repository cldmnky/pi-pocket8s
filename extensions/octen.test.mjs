// The Octen search adapter: an injected mock transport for the fixed endpoint, a loopback server
// only for the locally read URLs, and no real credential anywhere (the key below is a made-up
// string the mock may echo back).
//
// `octen.ts` imports nothing from Pi, so plain `node --test` exercises it. `make test` runs this.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { test } from "node:test";

import {
    formatOctenText,
    OCTEN_DEFAULT_COUNT,
    OCTEN_ENDPOINT,
    OCTEN_MAX_QUERY_CHARS,
    OCTEN_MAX_RESPONSE_BYTES,
    OctenError,
    octenApiKey,
    octenChoice,
    octenKeyNote,
    octenListEntry,
    octenPinProblem,
    octenShowText,
    readOctenPages,
    searchOcten,
} from "./web-search/octen.ts";
import { pinSource } from "./web-search/config.ts";

/** A credential that never existed: it exists to be redacted. */
const KEY = "octen-test-key-not-a-credential";

/** The documented success envelope, with two results. */
const HITS = {
    code: 0,
    msg: "ok",
    request_id: "req-1",
    data: {
        query: "octen search",
        results: [
            {
                title: "Octen docs",
                url: "https://docs.octen.ai/api-reference/search",
                highlight: "POST /search takes a query and a result count.",
                time_published: "2026-02-01T00:00:00Z",
                time_last_crawled: "2026-02-02T00:00:00Z",
            },
            {
                title: "A second page",
                url: "https://example.test/second",
                highlight: null,
                time_published: null,
                time_last_crawled: "2026-02-03T00:00:00Z",
            },
        ],
    },
    meta: { usage: { total_tokens: 0 }, warning: "" },
};

/** A transport that records what was sent and answers with the responder. */
function mockTransport(responder) {
    const requests = [];
    const fetchImpl = async (input, init = {}) => {
        const seen = {
            url: typeof input === "string" ? input : String(input),
            method: init.method,
            headers: init.headers ?? {},
            body: init.body,
            init,
        };

        requests.push(seen);

        return responder(seen);
    };

    return { requests, fetchImpl };
}

function json(value, status = 200, headers = {}) {
    return new Response(JSON.stringify(value), {
        status,
        headers: { "content-type": "application/json", ...headers },
    });
}

/** Run a search and return the error it threw, failing the test when it does not throw. */
async function failure(promise) {
    try {
        await promise;
    } catch (error) {
        assert.ok(error instanceof OctenError, `expected OctenError, got ${String(error)}`);
        return error;
    }

    assert.fail("the search was expected to fail");
}

/** Temporarily set environment variables, restoring them afterwards. */
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
    }
}

/** A loopback server for the URLs a search asked to read: nothing leaves the machine. */
async function loopback(handler) {
    const requests = [];
    const server = createServer((request, response) => {
        requests.push({ url: request.url, method: request.method });
        handler(request, response);
    });

    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));

    return {
        base: `http://127.0.0.1:${String(server.address().port)}`,
        requests,
        close: async () => {
            server.closeAllConnections();
            await new Promise((resolve) => server.close(resolve));
        },
    };
}

test("a search sends the documented request to the fixed endpoint and returns links and highlights", async () => {
    const net = mockTransport(() => json(HITS));

    const search = await searchOcten({ query: "octen search", apiKey: KEY, fetchImpl: net.fetchImpl });
    const sent = net.requests[0];

    assert.equal(net.requests.length, 1, "exactly one request: no retries");
    assert.equal(sent.url, OCTEN_ENDPOINT, "the address is fixed, not configurable");
    assert.equal(OCTEN_ENDPOINT, "https://api.octen.ai/search");
    assert.equal(sent.method, "POST");
    assert.equal(sent.headers.authorization, `Bearer ${KEY}`);
    assert.match(sent.headers["content-type"], /^application\/json/);
    assert.equal(sent.init.redirect, "manual", "redirects are never followed");
    assert.deepEqual(JSON.parse(sent.body), { query: "octen search", count: OCTEN_DEFAULT_COUNT });

    assert.equal(search.hits.length, 2);
    assert.equal(search.requestId, "req-1");
    assert.deepEqual(search.hits[0], {
        title: "Octen docs",
        url: "https://docs.octen.ai/api-reference/search",
        excerpt: "POST /search takes a query and a result count.",
        published: "2026-02-01T00:00:00Z",
        crawled: "2026-02-02T00:00:00Z",
    });
    assert.equal(search.hits[1].excerpt, undefined, "a null highlight is no excerpt");

    const text = formatOctenText(search);

    assert.match(text, /Octen search for "octen search": 2 results\./);
    assert.match(text, /1\. Octen docs\n   https:\/\/docs\.octen\.ai\/api-reference\/search/);
    assert.match(text, /POST \/search takes a query and a result count\./);
    assert.match(text, /published 2026-02-01T00:00:00Z, crawled 2026-02-02T00:00:00Z/);
    assert.doesNotMatch(text, new RegExp(KEY), "the answer never contains the credential");
});

test("a redirect is refused, not followed, and its Location is not quoted", async () => {
    const net = mockTransport(
        () => new Response(null, { status: 302, headers: { location: "https://elsewhere.test/search" } }),
    );

    const error = await failure(searchOcten({ query: "q", apiKey: KEY, fetchImpl: net.fetchImpl }));

    assert.equal(error.kind, "redirect");
    assert.equal(error.status, 302);
    assert.match(error.message, /answered a redirect \(HTTP 302\), which this tool never follows/);
    assert.match(error.message, /https:\/\/api\.octen\.ai\/search only/);
    assert.doesNotMatch(error.message, /elsewhere\.test/, "the Location header is not passed on");
    assert.equal(net.requests.length, 1, "no second request and no retry");
});

test("the result count is bounded to the documented 1..100 and the query to 500 characters", async () => {
    const net = mockTransport(() => json({ code: 0, data: { query: "q", results: [] } }));

    await searchOcten({ query: "q", count: 500, apiKey: KEY, fetchImpl: net.fetchImpl });
    await searchOcten({ query: "q", count: 0, apiKey: KEY, fetchImpl: net.fetchImpl });

    assert.equal(JSON.parse(net.requests[0].body).count, 100, "above the maximum is clamped");
    assert.equal(JSON.parse(net.requests[1].body).count, 1, "below the minimum is clamped");

    const long = "x".repeat(OCTEN_MAX_QUERY_CHARS + 1);
    const error = await failure(searchOcten({ query: long, apiKey: KEY, fetchImpl: net.fetchImpl }));

    assert.equal(error.kind, "input");
    assert.match(error.message, /501 characters; Octen accepts at most 500/);
    assert.equal(net.requests.length, 2, "an oversized query is refused before any request");

    const exactly = "y".repeat(OCTEN_MAX_QUERY_CHARS);
    await searchOcten({ query: exactly, apiKey: KEY, fetchImpl: net.fetchImpl });
    assert.equal(net.requests.length, 3, "exactly 500 characters is allowed");
});

test("an empty result list stays empty and never claims grounding", async () => {
    const net = mockTransport(() => json({ code: 0, msg: "ok", request_id: "req-empty", data: { query: "nothing", results: [] } }));
    const search = await searchOcten({ query: "nothing", apiKey: KEY, fetchImpl: net.fetchImpl });
    const text = formatOctenText(search);

    assert.deepEqual(search.hits, []);
    assert.match(text, /Octen returned no results for "nothing"\./);
    assert.match(text, /Nothing was grounded: no result came back/);
});

test("a warning from the service and dropped results are reported, not hidden", async () => {
    const net = mockTransport(() =>
        json({
            code: 0,
            data: {
                results: [
                    { title: "Good", url: "https://example.test/good" },
                    { title: "Script", url: "javascript:alert(1)" },
                    { title: "Credentials", url: "https://user:password@example.test/private" },
                    { title: "Relative", url: "/no-host" },
                    { title: "No address", highlight: "text only" },
                ],
            },
            meta: { warning: "Some sources were skipped." },
        }),
    );
    const search = await searchOcten({ query: "mixed", apiKey: KEY, fetchImpl: net.fetchImpl });
    const text = formatOctenText(search);

    assert.equal(search.hits.length, 1, "only the http(s) result without credentials is usable");
    assert.equal(search.skipped, 4);
    assert.match(text, /4 results had no usable http\(s\) address and were left out/);
    assert.match(text, /Octen warned: Some sources were skipped\./);
    assert.doesNotMatch(text, /user:password/, "the dropped address is not echoed back");
});

test("provider text is flattened and bounded before it reaches the answer", async () => {
    const net = mockTransport(() =>
        json({
            code: 0,
            data: {
                results: [
                    {
                        title: "Ti\u001b[31mtle\nwith a second line",
                        url: "https://example.test/page",
                        highlight: `${"long ".repeat(400)}\u0007`,
                    },
                ],
            },
        }),
    );
    const search = await searchOcten({ query: "odd text", apiKey: KEY, fetchImpl: net.fetchImpl });
    const hit = search.hits[0];

    assert.equal(hit.title, "Ti [31mtle with a second line", "control characters removed, one line");
    assert.ok(hit.excerpt.length <= 800, `excerpt is bounded (${String(hit.excerpt.length)})`);
    assert.doesNotMatch(hit.excerpt, /[\u0000-\u001f\u007f]/, "no control characters survive");
});

test("the credential is redacted from every returned field before any truncation", async () => {
    // 190 characters, then the key: a truncate-first implementation would leave part of the key.
    const padded = `${"p".repeat(190)}${KEY}${"q".repeat(80)}`;
    const net = mockTransport(() =>
        json({
            code: 0,
            request_id: `req-${KEY}`,
            data: {
                results: [
                    {
                        title: `title ${KEY}`,
                        url: `https://example.test/${KEY}/page`,
                        highlight: `highlight ${KEY}`,
                        time_published: `2026-02-01 ${KEY}`,
                        time_last_crawled: `2026-02-02 ${KEY}`,
                    },
                ],
            },
            meta: { warning: padded },
        }),
    );
    const search = await searchOcten({ query: "redact", apiKey: KEY, fetchImpl: net.fetchImpl });
    const serialized = JSON.stringify(search);

    assert.doesNotMatch(serialized, new RegExp(KEY), "no returned field holds the credential");
    assert.doesNotMatch(serialized, new RegExp(KEY.slice(0, 12)), "and no truncated prefix of it either");
    assert.match(search.hits[0].title, /\[redacted\]/);
    assert.match(search.hits[0].excerpt, /\[redacted\]/);
    assert.match(search.hits[0].url, /\[redacted\]/);
    assert.match(search.warning, /^p+\[redacted\]/, "the warning is redacted before it is cut");
    assert.match(search.requestId, /\[redacted\]/);
});

test("requested URLs are read locally, not sent to Octen", async () => {
    const net = mockTransport(() => json(HITS));
    const pages = await loopback((request, response) => {
        if (request.url === "/page") {
            response.writeHead(200, { "content-type": "text/html; charset=utf-8" });
            response.end("<html><head><title>Read me</title></head><body><p>Page body text.</p></body></html>");

            return;
        }

        if (request.url === "/binary") {
            response.writeHead(200, { "content-type": "application/octet-stream" });
            response.end(Buffer.from([0x89, 0x50, 0x4e, 0x47]));

            return;
        }

        response.writeHead(404, { "content-type": "text/plain" });
        response.end("nothing here");
    });

    try {
        const search = await searchOcten({ query: "octen search", apiKey: KEY, fetchImpl: net.fetchImpl });
        const read = await readOctenPages([`${pages.base}/page`, `${pages.base}/missing`, `${pages.base}/binary`, "not a url"]);
        const text = formatOctenText(search, read);

        assert.equal(read.length, 4);
        assert.equal(read[0].ok, true);
        assert.equal(read[0].title, "Read me");
        assert.match(read[0].text, /Page body text\./);
        assert.equal(read[1].ok, false);
        assert.match(read[1].reason, /answered HTTP 404/);
        assert.equal(read[2].ok, false);
        assert.match(read[2].reason, /application\/octet-stream is not text/);
        assert.equal(read[3].ok, false);
        assert.match(read[3].reason, /is not a URL/);

        assert.equal(net.requests.length, 1, "one Octen call; the URLs never went to Octen");
        assert.match(text, /Requested pages, read locally \(Octen does not fetch URLs\):/);
        assert.match(text, /### Read me/);
        assert.match(text, /Page body text\./);
        assert.match(text, /Could not be read: .*answered HTTP 404/);
    } finally {
        await pages.close();
    }
});

test("requested page text is capped, so a long answer cannot grow without bound", async () => {
    const net = mockTransport(() => json({ code: 0, data: { results: [] } }));
    const pages = await loopback((request, response) => {
        response.writeHead(200, { "content-type": "text/plain" });
        response.end("z".repeat(10_000));
    });

    try {
        const search = await searchOcten({ query: "q", apiKey: KEY, fetchImpl: net.fetchImpl });
        const read = await readOctenPages(Array.from({ length: 6 }, (_, index) => `${pages.base}/page${String(index)}`));
        const text = formatOctenText(search, read);

        assert.ok(read.every((page) => page.ok));
        assert.ok(Buffer.byteLength(text, "utf8") < 24 * 1024, `answer stays small (${String(Buffer.byteLength(text, "utf8"))} bytes)`);
        assert.match(text, /not quoted: the text reached the 16 KB this answer reads/);
    } finally {
        await pages.close();
    }
});

test("HTTP failures are reported as fixed sentences: no body, header or exception text", async () => {
    const cases = [
        [401, "auth", /rejected the API key \(HTTP 401\)/],
        [403, "forbidden", /refused this request \(HTTP 403\)/],
        [429, "rate-limit", /rate limited this request \(HTTP 429\)/],
        [500, "api", /answered HTTP 500/],
    ];

    for (const [status, kind, pattern] of cases) {
        const net = mockTransport(() =>
            // A hostile service might echo the key: none of this may reach the answer.
            new Response(JSON.stringify({ code: status, msg: `upstream detail ${KEY} retry 9999` }), {
                status,
                headers: {
                    "content-type": "application/json",
                    "retry-after": "9999",
                    "x-upstream-id": "header-secret-value",
                },
            }),
        );

        const error = await failure(searchOcten({ query: "q", apiKey: KEY, fetchImpl: net.fetchImpl }));

        assert.equal(error.kind, kind);
        assert.equal(error.status, status);
        assert.match(error.message, pattern);
        assert.doesNotMatch(error.message, /upstream detail/, "the error body is not quoted");
        assert.doesNotMatch(error.message, /9999/, "no header value is quoted");
        assert.doesNotMatch(error.message, /header-secret-value/);
        assert.doesNotMatch(error.message, new RegExp(KEY), "the credential is never quoted");
        assert.equal(net.requests.length, 1, "no retry after a billable failure");
    }
});

test("a nonzero code is an error even when the HTTP status is 200, and its message is not quoted", async () => {
    const net = mockTransport(() =>
        json({ code: 4001, msg: `plan does not include search (${KEY})`, request_id: "req-2" }),
    );
    const error = await failure(searchOcten({ query: "q", apiKey: KEY, fetchImpl: net.fetchImpl }));

    assert.equal(error.kind, "api");
    assert.equal(error.status, 200);
    assert.match(error.message, /returned error code 4001/);
    assert.doesNotMatch(error.message, /plan does not include search/);
    assert.doesNotMatch(error.message, new RegExp(KEY));
});

test("malformed and undocumented answers are refused, not guessed at or quoted", async () => {
    const shapes = [
        ["not json at all", /was not JSON/],
        ["[]", /was not the documented object/],
        [JSON.stringify({ msg: "no code" }), /no numeric code/],
        [JSON.stringify({ code: 0 }), /no data object/],
        [JSON.stringify({ code: 0, data: { results: "nope" } }), /no data\.results array/],
        [JSON.stringify({ code: 0, data: { results: [] }, meta: 5 }), /meta field that is not an object/],
    ];

    for (const [body, pattern] of shapes) {
        const net = mockTransport(
            () => new Response(body, { status: 200, headers: { "content-type": "application/json" } }),
        );
        const error = await failure(searchOcten({ query: "q", apiKey: KEY, fetchImpl: net.fetchImpl }));

        assert.equal(error.kind, "malformed", `for ${body}`);
        assert.match(error.message, pattern);
        assert.match(error.message, /Nothing was used/);
        assert.doesNotMatch(error.message, /not json at all|no code|nope/, "the body is not echoed");
    }
});

test("an oversized answer is refused rather than read whole", async () => {
    const net = mockTransport(
        () => new Response("x".repeat(OCTEN_MAX_RESPONSE_BYTES + 1), { status: 200 }),
    );
    const error = await failure(searchOcten({ query: "q", apiKey: KEY, fetchImpl: net.fetchImpl }));

    assert.equal(error.kind, "oversized");
    assert.match(error.message, /larger than the 2 MB this tool reads/);
});

test("a stalled request times out with the typed timeout error", async () => {
    const net = mockTransport(
        (seen) =>
            new Promise((_, reject) => {
                seen.init.signal.addEventListener("abort", () => reject(seen.init.signal.reason), { once: true });
            }),
    );
    const error = await failure(searchOcten({ query: "q", apiKey: KEY, fetchImpl: net.fetchImpl, timeoutMs: 300 }));

    assert.equal(error.kind, "timeout");
    assert.match(error.message, /did not answer within 300 ms/);
    assert.match(error.message, /not retried/);
});

test("a stalled body read is the same typed timeout, not a hang", async () => {
    const net = mockTransport(
        () =>
            new Response(
                new ReadableStream({
                    start() {
                        // Never enqueues and never closes: only the deadline can end this.
                    },
                }),
                { status: 200, headers: { "content-type": "application/json" } },
            ),
    );
    const error = await failure(searchOcten({ query: "q", apiKey: KEY, fetchImpl: net.fetchImpl, timeoutMs: 300 }));

    assert.equal(error.kind, "timeout");
    assert.match(error.message, /did not answer within 300 ms/);
});

test("a body read that fails is a fixed network error, not the stream's message", async () => {
    const net = mockTransport(
        () =>
            new Response(
                new ReadableStream({
                    start(controller) {
                        controller.error(new Error(`socket exploded with Bearer ${KEY}`));
                    },
                }),
                { status: 200, headers: { "content-type": "application/json" } },
            ),
    );
    const error = await failure(searchOcten({ query: "q", apiKey: KEY, fetchImpl: net.fetchImpl }));

    assert.equal(error.kind, "network");
    assert.match(error.message, /answer could not be read, so nothing was used/);
    assert.doesNotMatch(error.message, /socket exploded|Bearer/);
    assert.doesNotMatch(error.message, new RegExp(KEY));
});

test("a transport failure is reported without its own message", async () => {
    const net = mockTransport(() => {
        throw new TypeError(`fetch failed for https://api.octen.ai/search with header Bearer ${KEY}`);
    });
    const error = await failure(searchOcten({ query: "q", apiKey: KEY, fetchImpl: net.fetchImpl }));

    assert.equal(error.kind, "network");
    assert.match(error.message, /could not be reached, so nothing was searched/);
    assert.doesNotMatch(error.message, /fetch failed|api\.octen\.ai|Bearer/);
    assert.doesNotMatch(error.message, new RegExp(KEY));
});

test("a missing key is a configuration error and sends nothing", async () => {
    const net = mockTransport(() => json(HITS));

    await withEnv({ OCTEN_API_KEY: undefined }, async () => {
        assert.equal(octenApiKey(), undefined);

        const error = await failure(searchOcten({ query: "q", fetchImpl: net.fetchImpl }));

        assert.equal(error.kind, "config");
        assert.match(error.message, /needs OCTEN_API_KEY/);
        assert.match(error.message, /octen-api-key Secret/);
        assert.equal(net.requests.length, 0, "no request without a credential");

        const blank = await failure(searchOcten({ query: "q", apiKey: "   ", fetchImpl: net.fetchImpl }));
        assert.equal(blank.kind, "config");
    });
});

test("OCTEN_API_KEY is read at call time, and there is no endpoint override", async () => {
    const net = mockTransport(() => json(HITS));

    await withEnv({ OCTEN_API_KEY: ` ${KEY} ` }, async () => {
        assert.equal(octenApiKey(), KEY, "the key is trimmed");
        const search = await searchOcten({ query: "env", fetchImpl: net.fetchImpl });

        assert.equal(net.requests[0].url, OCTEN_ENDPOINT);
        assert.equal(net.requests[0].headers.authorization, `Bearer ${KEY}`);
        assert.equal(search.hits.length, 2);
    });

    // Nothing in the environment can move an Octen search off the fixed endpoint.
    await withEnv({ OCTEN_API_URL: "https://evil.test/search" }, async () => {
        await searchOcten({ query: "env", apiKey: KEY, fetchImpl: net.fetchImpl });
        assert.equal(net.requests.at(-1).url, OCTEN_ENDPOINT);
    });
});

test("the Octen pin is recognized without a model registry entry", () => {
    assert.deepEqual(octenChoice("octen", "search"), { kind: "octen" });
    assert.deepEqual(octenChoice("octen", "octen/search"), { kind: "octen" });
    assert.deepEqual(octenChoice(undefined, "octen/search"), { kind: "octen" });
    assert.deepEqual(octenChoice("OCTEN", "search"), { kind: "octen" });

    const wrong = octenChoice("octen", "web-search");
    assert.equal(wrong.kind, "octen-invalid");
    assert.match(wrong.problem, /one search operation/);

    const namespaced = octenChoice(undefined, "octen/web");
    assert.equal(namespaced.kind, "octen-invalid");

    // Every existing choice keeps its meaning: a provider's own model is untouched.
    assert.deepEqual(octenChoice(undefined, "gpt-5.6-sol"), { kind: "not-octen" });
    assert.deepEqual(octenChoice("openai-codex", "gpt-5.6-sol"), { kind: "not-octen" });
    assert.deepEqual(octenChoice(undefined, "anthropic/claude-haiku-4.5"), { kind: "not-octen" });
    assert.deepEqual(octenChoice("ollama-cloud", "gpt-oss:120b"), { kind: "not-octen" });

    assert.equal(octenPinProblem({ provider: "octen", model: "search" }), undefined);
    assert.match(octenPinProblem({ provider: "octen", model: "web" }), /one search operation/);
    assert.equal(octenPinProblem({ provider: "openai-codex", model: "gpt-5.6-sol" }), undefined);
});

test("the Octen choice is listed only when its key is present", () => {
    assert.equal(octenListEntry(false), undefined, "an unusable choice is not offered");
    assert.match(octenListEntry(true), /^- octen\/search \(direct Octen search API, no model call; OCTEN_API_KEY is set\)$/);
    assert.match(octenKeyNote(true), /^OCTEN_API_KEY is set\.$/);
    assert.match(octenKeyNote(false), /pod restart is needed/);
});

test("an Octen pin the portal owns is reported as the portal's", () => {
    const portal = "/run/pocket-config/web-search.json";
    const workspace = "/workspace/home/.pi/agent/web-search.json";

    assert.equal(pinSource(portal), "portal");
    assert.equal(pinSource(workspace), "workspace");

    const shown = octenShowText("set by the portal, in /run/pocket-config/web-search.json", true);

    assert.match(shown, /Octen search API \(octen\/search, set by the portal, in \/run\/pocket-config\/web-search\.json\)/);
    assert.doesNotMatch(shown, /Warning/);
});

test("an unusable Octen pin says how to fix it", () => {
    const shown = octenShowText("pinned in /tmp/web-search.json", false, octenPinProblem({ provider: "octen", model: "web" }));

    assert.match(shown, /Warning: The pin names Octen with model "web"/);
    assert.match(shown, /Searches fail until this is fixed/);
    assert.match(shown, /pod restart is needed/, "a missing key is still reported");
});
