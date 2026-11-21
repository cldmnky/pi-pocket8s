/**
 * Octen: a search API, not a model.
 *
 * Every other search this extension can run is a billable call to a *model* provider's own search
 * tool. Octen is different: `POST https://api.octen.ai/search` takes a query and a result count and
 * answers with links, so no model sits between the question and the results, and the install needs
 * no model registry entry for it.
 *
 * The documented contract this module is written against
 * (<https://docs.octen.ai/api-reference/search>):
 *
 *     POST /search          Authorization: Bearer <key>   (x-api-key works too)
 *     {"query": "…", "count": 1..100}
 *     {"code": 0, "msg": "…", "request_id": "…",
 *      "data": {"query": "…", "results": [{"title", "url", "highlight", "time_published", "time_last_crawled"}]},
 *      "meta": {"usage": {…}, "warning": "…"}}
 *
 * `code` is 0 on success; anything else is an error even when the HTTP status is 200. Query length is
 * capped at 500 characters and `count` at 1..100 by the service.
 *
 * Rules this module keeps:
 *
 * - **One destination.** The endpoint is `https://api.octen.ai/search`, always. There is no
 *   environment override, and a redirect is refused rather than followed: the key must only ever be
 *   sent there. Tests inject a mock transport (`fetchImpl`) instead of moving the address.
 * - **No silent retries.** A search is billable, so a failed call is reported, never repeated here,
 *   and the tool marks the call `replay: "unsafe"` — an interrupted search is not rerun either.
 * - **Never log the credential.** It goes into one request header. Every string that comes back from
 *   the service is redacted against it *before* it is truncated, so a cut can never leave half a key.
 * - **No upstream text in errors.** A status, a code and a fixed sentence only: no response body, no
 *   header value and no exception message is passed to the model.
 * - **Search content is untrusted.** Titles, excerpts and warnings are flattened, stripped of
 *   control characters and bounded before they go into the answer.
 * - **No grounding without results.** An empty `results` array stays empty; nothing claims a search
 *   happened when it did not.
 * - **The read is bounded**: response bytes, excerpt length, page count and the total page text. The
 *   body read is raced against the call's deadline, so a stalled body is a timeout, not a hang.
 *
 * Imports nothing from Pi, and only `fetch.ts` at runtime, so `node --test` exercises it directly.
 * `OCTEN_API_KEY` is the credential, mounted by the chart from the `octen-api-key` Secret when
 * `octen.enabled` is set; it is read at call time.
 */
import { cutText, FETCH_TIMEOUT_MS, FetchError, fetchPage } from "./fetch.ts";

/** The provider id a pin uses: `{"provider":"octen","model":"search"}` in `web-search.json`. */
export const OCTEN_PROVIDER = "octen";
/** Octen has one operation, so the pin's model is always this. */
export const OCTEN_MODEL = "search";
/** The only endpoint this module ever sends a request to. */
export const OCTEN_ENDPOINT = "https://api.octen.ai/search";

/** The service rejects a query longer than this. */
export const OCTEN_MAX_QUERY_CHARS = 500;
/** Default result count per search; the service takes 1..100. */
export const OCTEN_DEFAULT_COUNT = 10;
export const OCTEN_MAX_COUNT = 100;
/** A search is a small JSON call; 20 s is generous and keeps a hung request short. */
export const OCTEN_TIMEOUT_MS = 20_000;
/** Most bytes read from one answer. A 10-result page is a few KB. */
export const OCTEN_MAX_RESPONSE_BYTES = 2 * 1024 * 1024;
/** Bounds on what untrusted provider text can put into the answer. */
const OCTEN_MAX_TITLE_CHARS = 300;
const OCTEN_MAX_EXCERPT_CHARS = 800;
const OCTEN_MAX_TIME_CHARS = 40;
const OCTEN_MAX_WARNING_CHARS = 200;
const OCTEN_MAX_URL_CHARS = 2048;
const OCTEN_MAX_REQUEST_ID_CHARS = 120;
/** `urls` in web_search's schema allows 20. */
export const OCTEN_MAX_URLS = 20;
/** Most text taken from one requested page. */
export const OCTEN_MAX_PAGE_BYTES = 4000;
/** Most text taken from all requested pages together. */
export const OCTEN_MAX_PAGES_BYTES = 16 * 1024;

export type OctenErrorKind =
    | "config"
    | "input"
    | "auth"
    | "forbidden"
    | "rate-limit"
    | "redirect"
    | "api"
    | "malformed"
    | "oversized"
    | "network"
    | "timeout";

/** What went wrong, in words meant for the model and the person reading the answer. */
export class OctenError extends Error {
    readonly kind: OctenErrorKind;
    /** The HTTP status, when a response was received. */
    readonly status?: number;

    constructor(kind: OctenErrorKind, message: string, status?: number) {
        super(message);
        this.name = "OctenError";
        this.kind = kind;
        if (status !== undefined) this.status = status;
    }
}

/** One result, already cleaned, redacted and bounded. */
export interface OctenHit {
    title: string;
    url: string;
    excerpt?: string;
    published?: string;
    crawled?: string;
}

/** A successful search: the service answered `code: 0`. */
export interface OctenSearch {
    query: string;
    count: number;
    hits: OctenHit[];
    /** Results dropped because their address is not a usable http(s) URL. */
    skipped: number;
    warning?: string;
    requestId?: string;
}

/** One requested URL, read locally: Octen's search API has no URL-context operation. */
export interface OctenPage {
    url: string;
    ok: boolean;
    title?: string;
    text?: string;
    reason?: string;
}

export interface OctenSearchOptions {
    query: string;
    count?: number;
    /** Defaults to `OCTEN_API_KEY`. */
    apiKey?: string;
    /** The transport; production uses the global `fetch`, tests inject one. The address is fixed. */
    fetchImpl?: typeof fetch;
    signal?: AbortSignal;
    /** Defaults to `OCTEN_TIMEOUT_MS`. */
    timeoutMs?: number;
}

/** The credential, trimmed; undefined when it is absent or blank. */
export function octenApiKey(): string | undefined {
    const key = (process.env.OCTEN_API_KEY ?? "").trim();

    return key === "" ? undefined : key;
}

/** Whether a request to `web_search_config` names Octen, and whether that request is usable. */
export type OctenChoice =
    | { kind: "octen" }
    | { kind: "octen-invalid"; problem: string }
    | { kind: "not-octen" };

/**
 * Read a `web_search_config set` request as an Octen choice. A pin names Octen by provider, by
 * `octen/search`, or by the bare model id `search` together with `provider: "octen"`; Octen has one
 * operation, so any other model is a typo worth reporting instead of a pin that fails later.
 */
export function octenChoice(provider: string | undefined, model: string): OctenChoice {
    const named = provider?.trim().toLowerCase();
    const wanted = model.trim();

    if (named === OCTEN_PROVIDER) {
        return wanted === OCTEN_MODEL || wanted === `${OCTEN_PROVIDER}/${OCTEN_MODEL}`
            ? { kind: "octen" }
            : {
                  kind: "octen-invalid",
                  problem: `Octen has one search operation, so its model is "${OCTEN_MODEL}", not "${wanted}".`,
              };
    }

    // "octen/…" names the provider itself; another provider's namespaced id ("anthropic/claude…") is not ours.
    if (named === undefined || named === "") {
        if (wanted === `${OCTEN_PROVIDER}/${OCTEN_MODEL}`) return { kind: "octen" };
        if (wanted.startsWith(`${OCTEN_PROVIDER}/`)) {
            return {
                kind: "octen-invalid",
                problem: `Octen has one search operation, so its model is "octen/${OCTEN_MODEL}", not "${wanted}".`,
            };
        }
    }

    return { kind: "not-octen" };
}

/** Why a stored pin cannot search, when it names Octen with the wrong model. */
export function octenPinProblem(pin: { provider: string; model: string }): string | undefined {
    if (pin.provider.trim().toLowerCase() !== OCTEN_PROVIDER) return undefined;

    return pin.model.trim() === OCTEN_MODEL
        ? undefined
        : `The pin names Octen with model "${pin.model}", but Octen has one search operation: "${OCTEN_MODEL}".`;
}

/** One line for `web_search_config action=list`: Octen is a choice only when its key is present. */
export function octenListEntry(apiKeyPresent: boolean): string | undefined {
    if (!apiKeyPresent) return undefined;

    return `- ${OCTEN_PROVIDER}/${OCTEN_MODEL} (direct Octen search API, no model call; OCTEN_API_KEY is set)`;
}

/** What an answer must say about the credential: where it comes from, never its value. Never logs it. */
export function octenKeyNote(apiKeyPresent: boolean): string {
    return apiKeyPresent
        ? "OCTEN_API_KEY is set."
        : "OCTEN_API_KEY is not set on this install, so Octen searches fail until it is. The chart's octen.enabled mounts it from the octen-api-key Secret (field OCTEN-API-KEY); it is an environment variable, so a pod restart is needed after the Secret changes.";
}

/** `web_search_config action=show` for an Octen pin. */
export function octenShowText(owner: string, apiKeyPresent: boolean, problem?: string): string {
    const base = `Web search uses the Octen search API (${OCTEN_PROVIDER}/${OCTEN_MODEL}, ${owner}).`;
    const fix =
        problem === undefined
            ? ""
            : `\n\nWarning: ${problem} Searches fail until this is fixed: set ${OCTEN_PROVIDER}/${OCTEN_MODEL} with web_search_config action=set, or clear the pin.`;

    return `${base}${fix}\n\n${octenKeyNote(apiKeyPresent)}`;
}

/**
 * Search, once. Throws `OctenError` for every failure: a billable call is never retried here, so the
 * caller decides what to do next.
 */
export async function searchOcten(options: OctenSearchOptions): Promise<OctenSearch> {
    const query = options.query.trim();
    const length = Array.from(query).length;

    if (length === 0) {
        throw new OctenError("input", "web_search needs a query: Octen cannot search for nothing.");
    }

    if (length > OCTEN_MAX_QUERY_CHARS) {
        throw new OctenError(
            "input",
            `The query is ${length} characters; Octen accepts at most ${OCTEN_MAX_QUERY_CHARS}. Shorten it and search again. Nothing was sent.`,
        );
    }

    const apiKey = (options.apiKey ?? octenApiKey() ?? "").trim();

    if (apiKey === "") {
        throw new OctenError(
            "config",
            "Octen search needs OCTEN_API_KEY, which is not set in this pod. It comes from the octen-api-key Secret (field OCTEN-API-KEY) when the chart enables octen; a pod restart is needed after the Secret changes. Nothing was searched.",
        );
    }

    const count = bound(options.count ?? OCTEN_DEFAULT_COUNT, 1, OCTEN_MAX_COUNT);
    const timeoutMs = options.timeoutMs ?? OCTEN_TIMEOUT_MS;
    // One deadline for the whole call, and the caller's signal (a cancelled turn) still ends it.
    const timeout = AbortSignal.timeout(timeoutMs);
    const signal = options.signal === undefined ? timeout : AbortSignal.any([options.signal, timeout]);
    const transport = options.fetchImpl ?? fetch;

    let response: Response;

    try {
        response = await transport(OCTEN_ENDPOINT, {
            method: "POST",
            // Never follow a redirect: the key must only be sent to OCTEN_ENDPOINT.
            redirect: "manual",
            headers: {
                "content-type": "application/json",
                accept: "application/json",
                authorization: `Bearer ${apiKey}`,
            },
            body: JSON.stringify({ query, count }),
            signal,
        });
    } catch (error) {
        throw requestFailed(error, timeout, timeoutMs);
    }

    if (response.status >= 300 && response.status < 400) {
        await discard(response);

        throw new OctenError(
            "redirect",
            `Octen's endpoint answered a redirect (HTTP ${response.status}), which this tool never follows: a search goes to ${OCTEN_ENDPOINT} only. Nothing was used, and the call was not retried.`,
            response.status,
        );
    }

    if (!response.ok) {
        await discard(response);

        throw statusError(response.status);
    }

    const body = await collect(response, OCTEN_MAX_RESPONSE_BYTES, timeout, timeoutMs);

    if (body.overflow) {
        throw new OctenError(
            "oversized",
            `Octen's answer was larger than the ${formatBytes(OCTEN_MAX_RESPONSE_BYTES)} this tool reads, so nothing was used. Try a narrower query.`,
            response.status,
        );
    }

    return parseAnswer(body.text, query, count, apiKey, response.status);
}

/**
 * Read the URLs a `web_search` call asked for, locally and one at a time: Octen's search API does
 * not take URLs, and sending them to a model would be a second billable call. `fetchPage` supplies
 * the bounds — 5 redirects, 30 s, 5 MB — and the answer caps how much of each page is quoted.
 */
export async function readOctenPages(urls: readonly string[], signal?: AbortSignal): Promise<OctenPage[]> {
    const pages: OctenPage[] = [];

    for (const requested of urls.slice(0, OCTEN_MAX_URLS)) {
        try {
            // A deadline per page, so one slow address cannot use up the others' time.
            const each = AbortSignal.timeout(FETCH_TIMEOUT_MS);

            const page = await fetchPage(requested, {
                signal: signal === undefined ? each : AbortSignal.any([signal, each]),
            });
            pages.push(
                page.binary
                    ? {
                          url: page.url,
                          ok: false,
                          reason: `${page.contentType === "" ? "a binary file" : page.contentType} is not text; web_fetch returns text only.`,
                      }
                    : {
                          url: page.url,
                          ok: true,
                          ...(page.title === undefined ? {} : { title: page.title }),
                          text: page.text.trim(),
                      },
            );
        } catch (error) {
            pages.push({
                url: requested,
                ok: false,
                // `FetchError` is this repository's own wording; anything else stays unquoted.
                reason: error instanceof FetchError ? error.message : "the page could not be read",
            });
        }
    }

    return pages;
}

/** The answer text: what was found, and what was read. Never says more than came back. */
export function formatOctenText(search: OctenSearch, pages: readonly OctenPage[] = []): string {
    const lines: string[] = [];
    const found = search.hits.length;

    lines.push(
        found === 0
            ? `Octen returned no results for "${search.query}".`
            : `Octen search for "${search.query}": ${found} result${found === 1 ? "" : "s"}.`,
    );

    search.hits.forEach((hit, index) => {
        lines.push("", `${index + 1}. ${hit.title}`, `   ${hit.url}`);

        const when = [
            hit.published === undefined ? undefined : `published ${hit.published}`,
            hit.crawled === undefined ? undefined : `crawled ${hit.crawled}`,
        ].filter((part): part is string => part !== undefined);

        if (when.length > 0) lines.push(`   ${when.join(", ")}`);
        if (hit.excerpt !== undefined) lines.push(`   ${hit.excerpt}`);
    });

    if (found === 0) {
        lines.push("", "Nothing was grounded: no result came back, so do not present this as a finding about the web.");
    }

    if (search.skipped > 0) {
        lines.push(
            "",
            `(${search.skipped} result${search.skipped === 1 ? "" : "s"} had no usable http(s) address and ${search.skipped === 1 ? "was" : "were"} left out.)`,
        );
    }

    if (search.warning !== undefined) {
        lines.push("", `(Octen warned: ${search.warning})`);
    }

    if (pages.length > 0) {
        lines.push("", "Requested pages, read locally (Octen does not fetch URLs):");

        let budget = OCTEN_MAX_PAGES_BYTES;
        let quoted = 0;

        for (const page of pages) {
            if (!page.ok) {
                lines.push("", `### ${page.url}`, `Could not be read: ${page.reason ?? "no reason given"}`);
                continue;
            }

            if (budget <= 0) {
                quoted += 1;
                continue;
            }

            const text = cutText(page.text ?? "", Math.min(OCTEN_MAX_PAGE_BYTES, budget));

            budget -= Buffer.byteLength(text, "utf8");
            lines.push("", `### ${page.title ?? page.url}`, page.url, "", text === "" ? "(no readable text)" : text);
        }

        if (quoted > 0) {
            lines.push(
                "",
                `(${quoted} more requested page${quoted === 1 ? "" : "s"} not quoted: the text reached the ${formatBytes(OCTEN_MAX_PAGES_BYTES)} this answer reads.)`,
            );
        }
    }

    return lines.join("\n");
}

/** Every non-2xx status, as a fixed message: no body and no header value from the service is quoted. */
function statusError(status: number): OctenError {
    if (status === 401) {
        return new OctenError(
            "auth",
            "Octen rejected the API key (HTTP 401). Check OCTEN_API_KEY: it must be the octen-api-key Secret's OCTEN-API-KEY value. Nothing was searched, and the call was not retried.",
            status,
        );
    }

    if (status === 403) {
        return new OctenError(
            "forbidden",
            "Octen refused this request (HTTP 403). The key may not have access to the search API, or the account may be restricted. Nothing was retried.",
            status,
        );
    }

    if (status === 429) {
        return new OctenError(
            "rate-limit",
            "Octen rate limited this request (HTTP 429). A search is billable, so this tool does not retry it: try again later.",
            status,
        );
    }

    return new OctenError(
        "api",
        `Octen answered HTTP ${status}. Nothing was retried; if this repeats, check the Octen service status and the API key's plan.`,
        status,
    );
}

/** A request that never produced a response: a timeout, a cancelled turn, or a transport failure. */
function requestFailed(error: unknown, timeout: AbortSignal, timeoutMs: number): OctenError {
    if (timeout.aborted) return timeoutError(timeoutMs);

    if (error instanceof Error && error.name === "AbortError") {
        return new OctenError("network", "The Octen search was cancelled; nothing was retried.");
    }

    // The transport's own message is not passed on: it can name the address, headers or a cause.
    return new OctenError("network", "Octen could not be reached, so nothing was searched. Nothing was retried.");
}

/** The typed timeout, shared by the request, the body read and the caller's report. */
function timeoutError(timeoutMs: number): OctenError {
    const limit = timeoutMs >= 1000 && timeoutMs % 1000 === 0 ? `${timeoutMs / 1000} s` : `${timeoutMs} ms`;

    return new OctenError("timeout", `Octen did not answer within ${limit}. The call failed and was not retried.`);
}

/** Validate the documented envelope and read at most `count` results out of it. */
function parseAnswer(text: string, query: string, count: number, apiKey: string, status: number): OctenSearch {
    let body: unknown;

    try {
        body = JSON.parse(text);
    } catch {
        throw new OctenError(
            "malformed",
            `Octen's answer was not JSON (HTTP ${status}). Nothing was used, and the call was not retried.`,
            status,
        );
    }

    if (body === null || typeof body !== "object" || Array.isArray(body)) {
        throw new OctenError(
            "malformed",
            `Octen's answer was not the documented object (HTTP ${status}). Nothing was used.`,
            status,
        );
    }

    const envelope = body as Record<string, unknown>;
    const code = envelope.code;

    if (typeof code !== "number" || !Number.isInteger(code)) {
        throw new OctenError(
            "malformed",
            `Octen's answer has no numeric code (HTTP ${status}), so it cannot be read. Nothing was used.`,
            status,
        );
    }

    if (code !== 0) {
        // The code is a structured field and stays; `msg` is free text from the service and does not.
        throw new OctenError(
            "api",
            `Octen returned error code ${code}. Nothing was used, and the call was not retried.`,
            status,
        );
    }

    const data = envelope.data;

    if (data === null || typeof data !== "object" || Array.isArray(data)) {
        throw new OctenError("malformed", "Octen's answer has no data object. Nothing was used.", status);
    }

    const results = (data as Record<string, unknown>).results;

    if (!Array.isArray(results)) {
        throw new OctenError("malformed", "Octen's answer has no data.results array. Nothing was used.", status);
    }

    const hits: OctenHit[] = [];
    let skipped = 0;

    for (const entry of results) {
        if (hits.length >= count) break;

        if (entry === null || typeof entry !== "object" || Array.isArray(entry)) {
            skipped += 1;
            continue;
        }

        const record = entry as Record<string, unknown>;
        const address = clean(record.url, OCTEN_MAX_URL_CHARS, apiKey);
        let parsed: URL;

        try {
            if (address === undefined) throw new Error("no address");
            parsed = new URL(address);
        } catch {
            skipped += 1;
            continue;
        }

        if (
            (parsed.protocol !== "http:" && parsed.protocol !== "https:") ||
            parsed.username !== "" ||
            parsed.password !== ""
        ) {
            // Not something the model could fetch, and never a place for credentials in the answer.
            skipped += 1;
            continue;
        }

        const excerpt = clean(record.highlight, OCTEN_MAX_EXCERPT_CHARS, apiKey);
        const published = clean(record.time_published, OCTEN_MAX_TIME_CHARS, apiKey);
        const crawled = clean(record.time_last_crawled, OCTEN_MAX_TIME_CHARS, apiKey);

        hits.push({
            title: clean(record.title, OCTEN_MAX_TITLE_CHARS, apiKey) ?? parsed.toString(),
            url: parsed.toString(),
            ...(excerpt === undefined ? {} : { excerpt }),
            ...(published === undefined ? {} : { published }),
            ...(crawled === undefined ? {} : { crawled }),
        });
    }

    const meta = envelope.meta;

    if (meta !== null && meta !== undefined && (typeof meta !== "object" || Array.isArray(meta))) {
        throw new OctenError("malformed", "Octen's answer has a meta field that is not an object. Nothing was used.", status);
    }

    const warning = clean((meta as Record<string, unknown> | undefined)?.warning, OCTEN_MAX_WARNING_CHARS, apiKey);
    const requestId = clean(envelope.request_id, OCTEN_MAX_REQUEST_ID_CHARS, apiKey);

    return {
        // The query that was sent, not the service's echo: the answer says what was asked.
        query,
        count,
        hits,
        skipped,
        ...(warning === undefined ? {} : { warning }),
        ...(requestId === undefined ? {} : { requestId }),
    };
}

/** Read a response body, bounded, and raced against the call's deadline so a stalled body times out. */
async function collect(
    response: Response,
    limit: number,
    timeout: AbortSignal,
    timeoutMs: number,
): Promise<{ text: string; overflow: boolean }> {
    if (response.body === null) return { text: "", overflow: false };

    const reader = response.body.getReader();
    const chunks: Buffer[] = [];
    let size = 0;
    let overflow = false;
    let detach = (): void => undefined;
    const deadline = new Promise<never>((_, reject) => {
        const stop = (): void => reject(timeoutError(timeoutMs));

        if (timeout.aborted) {
            stop();
            return;
        }

        timeout.addEventListener("abort", stop, { once: true });
        detach = () => timeout.removeEventListener("abort", stop);
    });

    // The deadline may fire after the body is read; nobody awaits it then, so keep it handled.
    void deadline.catch(() => undefined);

    try {
        for (;;) {
            const { done, value } = await Promise.race([reader.read(), deadline]);

            if (done) break;

            const view = value instanceof Uint8Array ? value : new Uint8Array(value);

            if (size + view.byteLength > limit) {
                chunks.push(Buffer.from(view.subarray(0, limit - size)));
                size = limit;
                overflow = true;
                break;
            }

            chunks.push(Buffer.from(view));
            size += view.byteLength;
        }
    } catch (error) {
        try {
            await reader.cancel();
        } catch {
            // The stream may already be closed; nothing here is worth failing the report for.
        }

        if (error instanceof OctenError) throw error;

        throw new OctenError("network", "Octen's answer could not be read, so nothing was used. Nothing was retried.");
    } finally {
        detach();

        try {
            reader.releaseLock();
        } catch {
            // Already released by cancel.
        }
    }

    return { text: Buffer.concat(chunks).toString("utf8"), overflow };
}

/** Free the socket of a response whose body is not needed. */
async function discard(response: Response): Promise<void> {
    try {
        await response.body?.cancel();
    } catch {
        // The body may already be consumed or unusable; nothing here is worth failing a report for.
    }
}

/**
 * Untrusted provider text: the credential is removed first, so a later cut cannot leave part of it,
 * then control characters, then the length limit.
 */
function clean(value: unknown, limit: number, apiKey: string): string | undefined {
    if (typeof value !== "string") return undefined;

    const flat = redact(value, apiKey)
        .replace(/[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f]+/g, " ")
        .replace(/\s+/g, " ")
        .trim();

    return flat === "" ? undefined : cutText(flat, limit);
}

/** Remove the credential from anything that may reach the model. */
function redact(text: string, secret: string): string {
    return secret === "" ? text : text.split(secret).join("[redacted]");
}

function bound(value: number, minimum: number, maximum: number): number {
    if (!Number.isFinite(value)) return minimum;

    return Math.min(maximum, Math.max(minimum, Math.trunc(value)));
}

function formatBytes(bytes: number): string {
    return bytes >= 1024 * 1024 ? `${Math.round(bytes / (1024 * 1024))} MB` : `${Math.round(bytes / 1024)} KB`;
}
