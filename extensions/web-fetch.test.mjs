// web_fetch's module: URL rules, HTML reduction, and the real HTTP paths — all against a local
// server, so this suite needs no network. Plain `node --test`: fetch.ts imports nothing, which is
// the point of keeping the fetching here rather than in the tool.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { test } from "node:test";

import {
    cutText,
    decodeEntities,
    FetchError,
    fetchableUrl,
    fetchPage,
    htmlToText,
    MAX_BODY_BYTES,
} from "./web-search/fetch.ts";

/** Run `check` against a throwaway server, then close it even if the check throws. */
async function withServer(handler, check) {
    const server = createServer(handler);
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    const base = `http://127.0.0.1:${server.address().port}`;

    try {
        return await check(base);
    } finally {
        server.closeAllConnections();
        await new Promise((resolve) => server.close(resolve));
    }
}

test("cutText stops at the byte budget without splitting a character", () => {
    assert.equal(cutText("abcdef", 6), "abcdef");
    assert.equal(cutText("abcdef", 3), "abc");
    assert.equal(cutText("ééé", 4), "éé", "é is two bytes, so two of them fit in four");
    assert.equal(cutText("", 10), "");
});

test("only plain http(s) addresses are fetchable", () => {
    assert.equal(fetchableUrl("  https://example.com/a?b=c  ").toString(), "https://example.com/a?b=c");
    assert.equal(fetchableUrl("http://example.com/").protocol, "http:");

    assert.throws(() => fetchableUrl("not a url"), /is not a URL/);
    assert.throws(() => fetchableUrl("file:///etc/passwd"), /http and https/);
    assert.throws(() => fetchableUrl("data:text/plain,hello"), /http and https/);
    assert.throws(() => fetchableUrl("https://user:secret@example.com/"), /credentials/);

    assert.throws(() => fetchableUrl("file:///etc/passwd"), FetchError, "the rejection is ours, not TypeError");
});

test("HTML becomes the text a reader would see", () => {
    const html = `<!doctype html><html><head><title> Docs &amp; Notes </title>
        <style>p { color: red }</style><script>window.x = "&amp;";</script></head><body>
        <h1>Hello &nbsp;there</h1><p>First line<br>second &mdash; line</p>
        <ul><li>one</li><li>two</li></ul>
        <pre><code>def f():
    return &lt;div&gt;
</code></pre>
        <p>See <a href="/guide">the guide</a>, <a href="javascript:void(0)">nothing</a> and <a href="https://other.example/x">other</a>.</p>
        <table><tr><td>a</td><td>b</td></tr><tr><td>c</td><td>d</td></tr></table>
        <!-- a comment --><p>&#233;t&#xE9; &euro; &bogus;</p></body></html>`;

    const { title, text } = htmlToText(html, "https://example.com/page");

    assert.equal(title, "Docs & Notes");
    assert.match(text, /^Hello there$/m, "an entity space collapses like a rendered space");
    assert.match(text, /^First line\nsecond — line$/m);
    assert.match(text, /^- one\n- two$/m, "list items are lines of their own");
    assert.match(text, /^def f\(\):\n {4}return <div>$/m, "code keeps its lines, indentation and entities");
    assert.match(text, /\[the guide\]\(https:\/\/example\.com\/guide\)/, "relative links resolve");
    assert.match(text, /\[other\]\(https:\/\/other\.example\/x\)/);
    assert.match(text, /nothing/, "a javascript: link leaves its text alone");
    assert.doesNotMatch(text, /javascript:/);
    assert.match(text, /\ta\tb/, "table cells are tab-separated");
    assert.equal(text.split("\n").find((line) => line.includes("a\tb"))?.trim(), "a\tb");
    assert.match(text, /été € &bogus;$/, "numeric entities decode; an unknown name is left alone");
    assert.doesNotMatch(text, /color: red|window\.x|a comment/, "script, style and comments are dropped");
    assert.equal(text, htmlToText(html, "https://example.com/page").text, "reducing twice gives the same text");
    assert.equal(htmlToText("<p>no title here</p>").title, undefined, "a page without a title says so");
});

test("an unnamed entity that looks like a tag stays text, and a nameless pre still comes back", () => {
    const { text } = htmlToText("<p>Use &lt;div&gt; here</p><pre>one\n    two");

    assert.match(text, /Use <div> here/, "decoding happens after the markup is read");
    assert.match(text, /one\n {4}two/, "an unclosed pre keeps its contents");
});

test("entities decode by name, by number, and in the shape pages actually write them", () => {
    assert.equal(decodeEntities("&amp; &lt; &gt; &quot; &apos; &nbsp;"), "& < > \" '  ");
    assert.equal(decodeEntities("&#233; &#xE9; &#x1F600;"), "é é 😀");
    assert.equal(decodeEntities("&bogus; &amp"), "&bogus; &amp", "unknown or unterminated names stay as written");
    assert.equal(decodeEntities("&Auml; &auml; &copy; &hellip;"), "Ä ä © …", "a shouted name is still the name");
    assert.equal(decodeEntities("&#x110000; &#0; &#xD800;"), "&#x110000; &#0; &#xD800;", "impossible code points are left alone");
});

test("fetchPage reads a page: status, type, title, text and size", async () => {
    let userAgent = "";

    await withServer((request, response) => {
        userAgent = request.headers["user-agent"] ?? "";
        response.writeHead(200, { "content-type": "text/html; charset=utf-8" });
        response.end("<html><head><title>Page</title></head><body><p>hello</p></body></html>");
    }, async (base) => {
        const page = await fetchPage(`${base}/`);

        assert.equal(page.status, 200);
        assert.equal(page.contentType, "text/html; charset=utf-8");
        assert.equal(page.title, "Page");
        assert.equal(page.text, "hello");
        assert.equal(page.binary, false);
        assert.equal(page.truncated, false);
        assert.ok(page.bytes > 0, "the byte count is what came over the wire");
        assert.match(userAgent, /^pi-pocket-web-fetch/, "the address is told what is asking");
    });
});

test("a redirect is followed and the answer names the address that answered", async () => {
    await withServer((request, response) => {
        if (request.url === "/old") {
            response.writeHead(301, { location: "/new" });
            response.end();
            return;
        }

        response.writeHead(200, { "content-type": "text/plain" });
        response.end("moved");
    }, async (base) => {
        const page = await fetchPage(`${base}/old`);

        assert.equal(page.url, `${base}/new`);
        assert.equal(page.text, "moved");
    });
});

test("a redirect cycle stops, and a redirect out of http(s) is refused", async () => {
    await withServer((request, response) => {
        if (request.url === "/loop") {
            response.writeHead(302, { location: "/loop" });
            response.end();
            return;
        }

        response.writeHead(302, { location: "file:///etc/passwd" });
        response.end();
    }, async (base) => {
        await assert.rejects(fetchPage(`${base}/loop`), /too many redirects/);
        await assert.rejects(fetchPage(`${base}/elsewhere`), /will not follow/);
    });
});

test("an error status is reported with its code", async () => {
    await withServer((request, response) => {
        response.writeHead(404, { "content-type": "text/plain" });
        response.end("nope");
    }, async (base) => {
        await assert.rejects(fetchPage(`${base}/missing`), {
            name: "FetchError",
            message: /HTTP 404/,
        });
    });
});

test("a server that never answers stops on the timeout", async () => {
    await withServer(() => {
        // Deliberately never responds: this is the hanging-server case.
    }, async (base) => {
        await assert.rejects(
            fetchPage(`${base}/slow`, { signal: AbortSignal.timeout(250) }),
            /too long|timed out|stopped/,
        );
    });
});

test("JSON comes back as it is, and a body with no content type is read as text", async () => {
    const body = JSON.stringify({ ok: true, items: [1, 2, 3] });

    await withServer((request, response) => {
        if (request.url === "/api") {
            response.writeHead(200, { "content-type": "application/json" });
            response.end(body);
            return;
        }

        response.writeHead(200, {});
        response.end("plain, unlabelled");
    }, async (base) => {
        const json = await fetchPage(`${base}/api`);
        assert.equal(json.text, body, "a JSON body is passed through, not reformatted");
        assert.equal(json.binary, false);

        const bare = await fetchPage(`${base}/bare`);
        assert.equal(bare.text, "plain, unlabelled");
        assert.equal(bare.binary, false);
    });
});

test("a binary body is described, not dumped into the answer", async () => {
    await withServer((request, response) => {
        response.writeHead(200, { "content-type": "image/png", "content-length": "2048" });
        response.end(Buffer.alloc(2048, 7));
    }, async (base) => {
        const page = await fetchPage(`${base}/image.png`);

        assert.equal(page.binary, true);
        assert.equal(page.text, "");
        assert.equal(page.bytes, 2048, "the declared size is reported even though the body was not read");
        assert.equal(page.truncated, false);
    });
});

test("a binary body with no declared size reports none rather than zero", async () => {
    await withServer((request, response) => {
        response.writeHead(200, { "content-type": "application/pdf" });
        response.end("not really a pdf");
    }, async (base) => {
        const page = await fetchPage(`${base}/file.pdf`);

        assert.equal(page.binary, true);
        assert.equal(page.bytes, undefined, "chunked responses declare no size");
    });
});

test("a 204 answers with nothing, and says nothing", async () => {
    await withServer((request, response) => {
        response.writeHead(204);
        response.end();
    }, async (base) => {
        const page = await fetchPage(`${base}/empty`);

        assert.equal(page.status, 204);
        assert.equal(page.text, "");
        assert.equal(page.binary, false);
    });
});

test("the declared charset is used, and the page's own <meta> catches an unlabelled one", async () => {
    const accented = Buffer.from([0xe9]); // é in windows-1252

    await withServer((request, response) => {
        const head = request.url === "/header" ? "" : '<meta charset="windows-1252">';
        response.writeHead(200, { "content-type": request.url === "/header" ? "text/html; charset=windows-1252" : "text/html" });
        response.end(Buffer.concat([Buffer.from(`<html><head>${head}<title>caf`), accented, Buffer.from("</title></head><body>caf"), accented, Buffer.from("</body></html>")]));
    }, async (base) => {
        assert.equal((await fetchPage(`${base}/header`)).text, "café");
        assert.equal((await fetchPage(`${base}/meta`)).text, "café");
    });
});

test("a body longer than the cap is cut at the cap, and the page says so", async () => {
    const chunk = Buffer.alloc(64 * 1024, "a");
    const writes = Math.ceil((MAX_BODY_BYTES + 4 * 1024) / chunk.length);

    await withServer((request, response) => {
        response.writeHead(200, { "content-type": "text/plain" });
        for (let i = 0; i < writes; i += 1) {
            response.write(chunk);
        }
        response.end();
    }, async (base) => {
        const page = await fetchPage(`${base}/big`);

        assert.equal(page.truncated, true);
        assert.equal(page.bytes, MAX_BODY_BYTES);
        assert.equal(page.text.length, MAX_BODY_BYTES);
    });
});
