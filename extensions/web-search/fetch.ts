/**
 * web_fetch's plumbing: fetch one http(s) address and turn its body into readable text.
 *
 * Deliberately ordinary code — plain `fetch`, no vendored provider and no model call — because
 * fetching a page is a network read: it costs nothing, works whatever model the install has
 * configured, and the caller asked for the page itself rather than another model's summary of it.
 * HTML is reduced here (tags, entities, links, code blocks) with the byte cap applied while the
 * body streams in, so an enormous or merely broken page cannot balloon the process.
 *
 * This module imports nothing, which is what lets `node --test` exercise it outside Pi.
 */

/** The whole request — redirects included — must finish inside this. */
export const FETCH_TIMEOUT_MS = 30_000;
/** Redirect hops followed before giving up, so a redirect cycle cannot spin forever. */
export const MAX_REDIRECTS = 5;
/** Most bytes read from the network per fetch. The tool caps the text it returns separately. */
export const MAX_BODY_BYTES = 5 * 1024 * 1024;

/** What the address is told about us. A plain, honest agent string, not a browser's. */
const USER_AGENT = "pi-pocket-web-fetch (+https://github.com/cldmnky/pi-pocket8s)";
/** Order only hints: the server decides what it actually sends. */
const ACCEPT =
    "text/html,application/xhtml+xml,application/json;q=0.9,text/plain;q=0.9,application/xml;q=0.8,*/*;q=0.5";

/** A fetch that did not produce a page, with a message meant for the model to read and act on. */
export class FetchError extends Error {
    url?: string;

    constructor(message: string, url?: string) {
        super(message);
        this.name = "FetchError";
        this.url = url;
    }
}

export interface FetchedPage {
    /** The address that answered, after redirects. */
    url: string;
    status: number;
    /** The response's content type, or "" when it declared none. */
    contentType: string;
    /** Decoded readable text; empty when the body is binary not text. */
    text: string;
    /** The page's <title>, when it has one. */
    title?: string;
    /** The body is not a text format: only its type and size are reported. */
    binary: boolean;
    /** Size in bytes: what was read, or for a binary body what the address declared. */
    bytes?: number;
    /** Reading stopped at MAX_BODY_BYTES; the page continues past what came back. */
    truncated: boolean;
}

/** Reject anything web_fetch will not fetch, with a reason the model can pass on. */
export function fetchableUrl(raw: string): URL {
    let url: URL;

    try {
        url = new URL(raw.trim());
    } catch {
        throw new FetchError(
            `"${raw.trim()}" is not a URL. Give an absolute address such as https://example.com/page.`,
        );
    }

    if (url.protocol !== "http:" && url.protocol !== "https:") {
        throw new FetchError(
            `web_fetch fetches http and https addresses, not ${url.protocol}//: ${url.toString()}`,
        );
    }

    if (url.username !== "" || url.password !== "") {
        throw new FetchError(
            "The URL embeds credentials; web_fetch does not send those. Fetch the same address without user:password@.",
        );
    }

    return url;
}

/** Cut text to at most `maxBytes` UTF-8 bytes, backing off so no character is split in half. */
export function cutText(text: string, maxBytes: number): string {
    const bytes = Buffer.from(text, "utf8");

    return bytes.byteLength <= maxBytes ? text : bytes.subarray(0, maxBytes).toString("utf8");
}

/**
 * Fetch one address and decode it. Redirects are followed by hand rather than by `fetch`, so every
 * hop passes the same scheme check and the count is bounded; the timeout covers all of them.
 */
export async function fetchPage(rawUrl: string, options: { signal?: AbortSignal } = {}): Promise<FetchedPage> {
    const signal = options.signal ?? AbortSignal.timeout(FETCH_TIMEOUT_MS);
    const timeoutMs = options.signal === undefined ? FETCH_TIMEOUT_MS : undefined;
    let current = fetchableUrl(rawUrl);

    for (let hop = 0; ; hop += 1) {
        let response: Response;

        try {
            response = await fetch(current, {
                redirect: "manual",
                signal,
                headers: { "user-agent": USER_AGENT, accept: ACCEPT, "accept-language": "en;q=0.9,*;q=0.5" },
            });
        } catch (error) {
            throw new FetchError(describeFailure(current, error, timeoutMs), current.toString());
        }

        const location = response.headers.get("location");

        if (response.status >= 300 && response.status < 400 && location !== null) {
            await discard(response);

            if (hop >= MAX_REDIRECTS) {
                throw new FetchError(
                    `Gave up after ${MAX_REDIRECTS + 1} addresses: too many redirects, the last one being ${current.toString()}.`,
                    current.toString(),
                );
            }

            try {
                current = fetchableUrl(new URL(location, current).toString());
            } catch (error) {
                throw new FetchError(
                    `Redirected to an address web_fetch will not follow: ${(error as Error).message}`,
                    current.toString(),
                );
            }

            continue;
        }

        if (response.status < 200 || response.status >= 400) {
            await discard(response);
            const statusText = response.statusText === "" ? "" : ` ${response.statusText}`;
            throw new FetchError(
                `${current.toString()} answered HTTP ${response.status}${statusText}.`,
                current.toString(),
            );
        }

        const contentType = (response.headers.get("content-type") ?? "").replace(/\s+/g, " ").trim();
        const declaredHeader = response.headers.get("content-length");
        const declared = declaredHeader === null ? undefined : Number(declaredHeader);
        const kind = classify(contentType);

        if (response.body === null || response.status === 204 || response.status === 205) {
            await discard(response);
            return {
                url: current.toString(),
                status: response.status,
                contentType,
                text: "",
                binary: false,
                truncated: false,
            };
        }

        if (kind === "binary") {
            await discard(response);
            return {
                url: current.toString(),
                status: response.status,
                contentType,
                text: "",
                binary: true,
                ...(declared !== undefined && Number.isFinite(declared) && declared >= 0 ? { bytes: declared } : {}),
                truncated: false,
            };
        }

        let body: { bytes: Uint8Array; truncated: boolean };

        try {
            body = await readCapped(response.body, MAX_BODY_BYTES);
        } catch (error) {
            throw new FetchError(describeFailure(current, error, timeoutMs), current.toString());
        }

        const decoded = decodeBody(body.bytes, contentType);
        const page = kind === "markup" || looksLikeMarkup(decoded) ? htmlToText(decoded, current) : { title: undefined, text: decoded };

        return {
            url: current.toString(),
            status: response.status,
            contentType,
            text: page.text,
            ...(page.title === undefined ? {} : { title: page.title }),
            binary: false,
            bytes: body.bytes.byteLength,
            truncated: body.truncated,
        };
    }
}

async function discard(response: Response): Promise<void> {
    try {
        await response.body?.cancel();
    } catch {
        // The body may already be consumed or unusable; nothing here is worth failing the fetch for.
    }
}

/** Read at most `cap` bytes; breaking out of the loop cancels the rest of the download. */
async function readCapped(body: ReadableStream<Uint8Array>, cap: number): Promise<{ bytes: Uint8Array; truncated: boolean }> {
    const chunks: Uint8Array[] = [];
    let size = 0;
    let truncated = false;

    for await (const chunk of body as unknown as AsyncIterable<Uint8Array | ArrayBuffer>) {
        const view = chunk instanceof Uint8Array ? chunk : new Uint8Array(chunk);

        if (size + view.byteLength >= cap) {
            chunks.push(view.subarray(0, cap - size));
            truncated = size + view.byteLength > cap;
            size = cap;
            break;
        }

        chunks.push(view);
        size += view.byteLength;
    }

    const bytes = new Uint8Array(size);
    let offset = 0;

    for (const chunk of chunks) {
        bytes.set(chunk, offset);
        offset += chunk.byteLength;
    }

    return { bytes, truncated };
}

/** What the address serves decides how the body is read: markup, text, or not at all. */
function classify(contentType: string): "markup" | "text" | "unknown" | "binary" {
    const type = (contentType.split(";")[0] ?? "").trim().toLowerCase();

    if (type === "text/html" || type === "application/xhtml+xml" || type === "text/xml" || type === "application/xml") {
        return "markup";
    }

    if (type.startsWith("text/") || type === "application/json" || type.endsWith("+json")) {
        return "text";
    }

    // No content type at all is common enough to treat as text and sniff below.
    return type === "" ? "unknown" : "binary";
}

/** The first bytes of an unlabelled body decide whether it is markup worth reducing. */
function looksLikeMarkup(text: string): boolean {
    return /^\s*(?:<!doctype\s+html|<html|<head|<body|<meta|<title|<div|<p[\s>])/i.test(text.slice(0, 1024));
}

/** Respect the charset the address declares, then the page's own <meta>, then UTF-8. */
function decodeBody(bytes: Uint8Array, contentType: string): string {
    const declared = /charset\s*=\s*["']?([\w.:-]+)/i.exec(contentType)?.[1];
    // An unlabelled page usually names its charset in the markup, in the first bytes.
    const sniffed = declared === undefined
        ? /<meta[^>]+charset\s*=\s*["']?\s*([\w.:-]+)/i.exec(new TextDecoder("latin1").decode(bytes.subarray(0, 4096)))?.[1]
        : undefined;
    const label = declared ?? sniffed;

    if (label !== undefined) {
        try {
            return new TextDecoder(label).decode(bytes);
        } catch {
            // An unknown label is not fatal; UTF-8 is the web's default encoding.
        }
    }

    return new TextDecoder("utf-8").decode(bytes);
}

/** Say what went wrong in terms the model can act on, without leaking a raw undici object. */
function describeFailure(url: URL, error: unknown, timeoutMs?: number): string {
    const name = (error as { name?: string } | undefined)?.name;

    if (name === "TimeoutError") {
        return timeoutMs === undefined
            ? `Fetching ${url.toString()} took too long and was stopped.`
            : `Fetching ${url.toString()} timed out after ${Math.round(timeoutMs / 1000)} seconds.`;
    }

    if (name === "AbortError") {
        return `Fetching ${url.toString()} was stopped before it answered.`;
    }

    const cause = (error as { cause?: unknown } | undefined)?.cause;
    const detail = cause instanceof Error ? cause.message : (error as Error | undefined)?.message;
    const code = cause !== undefined && typeof (cause as { code?: unknown }).code === "string" ? (cause as { code: string }).code : undefined;
    const described = detail === undefined || detail === "" ? "no reason given" : detail;

    return `Could not reach ${url.toString()}: ${described}${code !== undefined && !described.includes(code) ? ` (${code})` : ""}.`;
}

const MARKUP_DROP = "script|style|noscript|template|svg|canvas|head|iframe|object|embed";

/** Elements whose own line is where their contents belong, on both sides. */
const BLOCK_ELEMENTS = new Set([
    "address", "article", "aside", "blockquote", "dd", "details", "dialog", "div", "dl", "dt",
    "fieldset", "figcaption", "figure", "footer", "form", "h1", "h2", "h3", "h4", "h5", "h6",
    "header", "hgroup", "legend", "main", "nav", "ol", "p", "section", "summary", "table",
    "tbody", "tfoot", "thead", "ul",
]);

/**
 * Reduce HTML to the text a reader would see: headings and list items on their own lines, links as
 * `[text](url)`, code blocks kept verbatim (placeholder-swapped while whitespace is collapsed),
 * entities decoded. Everything in a script, style or head is dropped.
 */
export function htmlToText(html: string, baseUrl?: string | URL): { title: string | undefined; text: string } {
    const title = inline(matchTitle(html)) || undefined;
    let body = html
        .replace(/<!--[\s\S]*?-->/g, "")
        .replace(new RegExp(`<(${MARKUP_DROP})\\b[^>]*>[\\s\\S]*?<\\/\\1\\s*>`, "gi"), " ")
        .replace(new RegExp(`<\\/?\\s*(?:${MARKUP_DROP})\\b[^>]*>`, "gi"), " ");

    const pre: string[] = [];
    let out = "";
    let insidePre = false;
    let code = "";
    let last = 0;
    const anchors: (string | undefined)[] = [];

    for (const match of body.matchAll(/<[^>]*>/g)) {
        const chunk = body.slice(last, match.index);

        // Inside <pre> the whitespace is the content: keep it verbatim, entities and all.
        if (insidePre) {
            code += chunk;
        } else {
            out += chunk.replace(/\s+/g, " ");
        }

        last = match.index + match[0].length;

        const tag = describeTag(match[0]);

        if (insidePre) {
            if (tag.closing && tag.name === "pre") {
                pre.push(code);
                out += `\u0000PRE${pre.length - 1}\u0000`;
                code = "";
                insidePre = false;
            }

            continue;
        }

        if (tag.closing) {
            if (tag.name === "a" && anchors.length > 0) {
                const href = anchors.pop();

                if (href !== undefined) {
                    out += `](${href})`;
                }
            } else if (BLOCK_ELEMENTS.has(tag.name)) {
                out += "\n";
            }

            continue;
        }

        if (tag.name === "pre") {
            insidePre = true;
        } else if (tag.name === "br" || tag.name === "hr" || tag.name === "tr") {
            out += "\n";
        } else if (tag.name === "li") {
            out += "\n- ";
        } else if (tag.name === "td" || tag.name === "th") {
            out += "\t";
        } else if (tag.name === "a") {
            const href = resolveLink(attribute(match[0], "href"), baseUrl);

            if (href === undefined) {
                anchors.push(undefined);
            } else {
                anchors.push(href);
                out += "[";
            }
        } else if (BLOCK_ELEMENTS.has(tag.name)) {
            out += "\n";
        }
    }

    if (insidePre) {
        // An unclosed <pre> keeps the rest of the document rather than dropping it.
        code += body.slice(last);
        pre.push(code);
        out += `\u0000PRE${pre.length - 1}\u0000`;
    } else {
        out += body.slice(last);
    }

    // Entities are decoded last, so `&lt;div&gt;` in the prose stays text. Code blocks are still
    // placeholders here; that is what keeps these passes from touching their indentation.
    const text = decodeEntities(
        out
            .replace(/\r\n?/g, "\n")
            .replace(/[ \t]+\n/g, "\n")
            .replace(/[ \t]{2,}/g, " ")
            .replace(/ *\n */g, "\n")
            .replace(/\n{3,}/g, "\n\n"),
    )
        // `&nbsp;` and friends become spaces only now; collapse the ones that doubled up.
        .replace(/[ \t]{2,}/g, " ")
        .replace(/ *\n */g, "\n")
        .replace(/\u0000PRE(\d+)\u0000/g, (_, index: string) => `\n\n${decodeEntities(pre[Number(index)] ?? "").trim()}\n\n`)
        .replace(/\[\s*\]\(\S*?\)/g, "")
        .replace(/\n{3,}/g, "\n\n")
        .trim();

    return { title, text };
}

function matchTitle(html: string): string {
    return /<title[^>]*>([\s\S]*?)<\/title\s*>/i.exec(html)?.[1] ?? "";
}

/** One tag's kind, without a parser: its name and whether it closes. */
function describeTag(raw: string): { name: string; closing: boolean } {
    const match = /^<\s*(\/?)\s*([a-zA-Z][a-zA-Z0-9:-]*)/.exec(raw);

    return { name: (match?.[2] ?? "").toLowerCase(), closing: match?.[1] === "/" };
}

/** Read an attribute value, quoted or bare. */
function attribute(raw: string, name: string): string | undefined {
    const match = new RegExp(`${name}\\s*=\\s*(?:"([^"]*)"|'([^']*)'|([^\\s"'>]+))`, "i").exec(raw);

    return match === null ? undefined : (match[1] ?? match[2] ?? match[3]);
}

/**
 * Link targets become markdown destinations, but only when they are http(s) after resolution:
 * `#anchor` and `javascript:` are navigation, not a document to fetch later.
 */
function resolveLink(href: string | undefined, baseUrl?: string | URL): string | undefined {
    if (href === undefined || href.trim() === "") {
        return undefined;
    }

    try {
        const url = new URL(decodeEntities(href.trim()), baseUrl);
        return url.protocol === "http:" || url.protocol === "https:" ? url.toString() : undefined;
    } catch {
        return undefined;
    }
}

/** Collapse runs of whitespace, as a browser would render them. */
function inline(text: string): string {
    return decodeEntities(text).replace(/\s+/g, " ").trim();
}

// The entities a page actually uses: markup, punctuation, currency, the accented letters, and the
// Greek alphabet. Numeric references cover the rest, and an unknown name is left as written.
const ENTITIES = new Map<string, string>([
    ["amp", "&"], ["lt", "<"], ["gt", ">"], ["quot", '"'], ["apos", "'"], ["nbsp", " "],
    ["copy", "©"], ["reg", "®"], ["trade", "™"], ["hellip", "…"], ["mdash", "—"], ["ndash", "–"],
    ["lsquo", "‘"], ["rsquo", "’"], ["ldquo", "“"], ["rdquo", "”"], ["laquo", "«"], ["raquo", "»"],
    ["times", "×"], ["divide", "÷"], ["middot", "·"], ["bull", "•"], ["deg", "°"], ["plusmn", "±"],
    ["minus", "−"], ["micro", "µ"], ["para", "¶"], ["sect", "§"], ["not", "¬"], ["uml", "¨"],
    ["frac12", "½"], ["frac14", "¼"], ["frac34", "¾"], ["sup2", "²"], ["sup3", "³"], ["prime", "′"],
    ["cent", "¢"], ["pound", "£"], ["yen", "¥"], ["euro", "€"], ["curren", "¤"], ["brvbar", "¦"],
    ["iexcl", "¡"], ["iquest", "¿"], ["szlig", "ß"], ["thorn", "þ"], ["eth", "ð"], ["ensp", " "],
    ["emsp", " "], ["thinsp", " "], ["zwnj", ""], ["zwj", ""], ["lrm", ""], ["rlm", ""],
    ["agrave", "à"], ["aacute", "á"], ["acirc", "â"], ["atilde", "ã"], ["auml", "ä"], ["aring", "å"],
    ["aelig", "æ"], ["ccedil", "ç"], ["egrave", "è"], ["eacute", "é"], ["ecirc", "ê"], ["euml", "ë"],
    ["igrave", "ì"], ["iacute", "í"], ["icirc", "î"], ["iuml", "ï"], ["ntilde", "ñ"], ["ograve", "ò"],
    ["oacute", "ó"], ["ocirc", "ô"], ["otilde", "õ"], ["ouml", "ö"], ["oslash", "ø"], ["ugrave", "ù"],
    ["uacute", "ú"], ["ucirc", "û"], ["uuml", "ü"], ["yacute", "ý"], ["yuml", "ÿ"],
    ["alpha", "α"], ["beta", "β"], ["gamma", "γ"], ["delta", "δ"], ["epsilon", "ε"], ["zeta", "ζ"],
    ["eta", "η"], ["theta", "θ"], ["iota", "ι"], ["kappa", "κ"], ["lambda", "λ"], ["mu", "μ"],
    ["nu", "ν"], ["xi", "ξ"], ["pi", "π"], ["rho", "ρ"], ["sigma", "σ"], ["tau", "τ"], ["upsilon", "υ"],
    ["phi", "φ"], ["chi", "χ"], ["psi", "ψ"], ["omega", "ω"],
    ["larr", "←"], ["rarr", "→"], ["uarr", "↑"], ["darr", "↓"], ["harr", "↔"], ["infin", "∞"],
    ["ne", "≠"], ["le", "≤"], ["ge", "≥"], ["sum", "∑"], ["prod", "∏"], ["radic", "√"], ["part", "∂"],
    ["int", "∫"], ["sim", "∼"], ["asymp", "≈"], ["equiv", "≡"], ["sdot", "⋅"],
]);

/** Decode named and numeric character references. An unknown name is left as written. */
export function decodeEntities(text: string): string {
    return text.replace(/&(#[0-9]+|#[xX][0-9a-fA-F]+|[a-zA-Z][a-zA-Z0-9]{1,31});/g, (whole, body: string) => {
        if (body.startsWith("#")) {
            const code = body[1] === "x" || body[1] === "X" ? Number.parseInt(body.slice(2), 16) : Number.parseInt(body.slice(1), 10);

            return Number.isInteger(code) && code > 0 && code <= 0x10ffff && !(code >= 0xd800 && code <= 0xdfff)
                ? String.fromCodePoint(code)
                : whole;
        }

        const direct = ENTITIES.get(body);

        if (direct !== undefined) {
            return direct;
        }

        // `&Auml;` is `&auml;` shouted; the rest of the case-insensitivity is a harmless fallback.
        const lower = ENTITIES.get(body.toLowerCase());

        return lower === undefined ? whole : (body[0] === body[0]?.toUpperCase() ? lower.toUpperCase() : lower);
    });
}
