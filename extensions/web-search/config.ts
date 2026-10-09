/**
 * The pin: which model searches the web, kept in Pi's own `web-search.json` — the file the vendored
 * provider code reads. Reading and writing it is all this does, so a person can set the same thing
 * by hand with an editor, and the two can never disagree.
 *
 * Imports nothing but Node, so `web-search.test.mjs` can exercise it in a temp directory.
 */
import { closeSync, mkdirSync, openSync, readFileSync, renameSync, rmSync, writeSync } from "node:fs";
import { dirname, join } from "node:path";

export type Pin = { provider: string; model: string };

export type PinFile = {
    path: string;
    /** The pin in effect, when the file holds a usable one. */
    pin?: Pin;
    /** Why the file is unusable, when it exists but does not hold a pin. */
    error?: string;
};

/** `PI_WEB_SEARCH_CONFIG` moves the file, exactly as the vendored code expects. */
export function pinPath(agentDir: string): string {
    return process.env.PI_WEB_SEARCH_CONFIG || join(agentDir, "web-search.json");
}

/** Where the runtime Secret is mounted into the agent; the portal writes the pin there. */
export const PORTAL_PIN_DIR = "/run/pocket-config/";

export type PinSource = "portal" | "workspace";

/** The portal's mounted file, or the workspace's own file. */
export function pinSource(path: string): PinSource {
    return path.startsWith(PORTAL_PIN_DIR) ? "portal" : "workspace";
}

/**
 * Why writing the pin failed, in terms a person can act on. The portal's file is on a read-only
 * mount, so a session cannot take the choice over: say who owns it and where to change it.
 */
export function describeWriteFailure(path: string, error: unknown): string {
    if (pinSource(path) === "portal") {
        return `The portal sets the search model for this workspace (${path} is read-only): change it in the portal's Web search setting, which applies after the agent restarts.`;
    }

    const code = (error as { code?: string }).code;

    if (code === "EROFS" || code === "EACCES" || code === "EPERM") {
        return `Cannot write ${path} (${code}): the agent cannot change the search model here.`;
    }

    return `Cannot write ${path}: ${describe(error)}`;
}

export function readPin(path: string): PinFile {
    let raw: string;

    try {
        raw = readFileSync(path, "utf8");
    } catch (error) {
        const code = (error as { code?: string }).code;
        return code === "ENOENT" ? { path } : { path, error: describe(error) };
    }

    let parsed: unknown;

    try {
        parsed = JSON.parse(raw);
    } catch (error) {
        return { path, error: `invalid JSON (${describe(error)})` };
    }

    if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
        return { path, error: "expected a JSON object with provider and model" };
    }

    const { provider, model, modelId } = parsed as Record<string, unknown>;
    const id = model ?? modelId;

    if (typeof provider !== "string" || provider.trim() === "" || typeof id !== "string" || id.trim() === "") {
        return { path, error: "needs a non-empty provider and model" };
    }

    return { path, pin: { provider: provider.trim(), model: id.trim() } };
}

/** Write the pin for the next search. Written to a temporary file and renamed, so a search that
 *  starts while this runs reads either the old file or the new one, never half of either. */
export function writePin(path: string, pin: Pin): void {
    mkdirSync(dirname(path), { recursive: true });

    const temporary = `${path}.${process.pid}.tmp`;
    const body = `${JSON.stringify({ provider: pin.provider, model: pin.model }, undefined, 2)}\n`;
    const fd = openSync(temporary, "w", 0o600);

    try {
        writeSync(fd, body);
    } finally {
        closeSync(fd);
    }

    renameSync(temporary, path);
}

/** Remove the pin: searches go back to the best available model. Returns whether one was there. */
export function clearPin(path: string): boolean {
    const before = readPin(path);

    rmSync(path, { force: true });

    return before.pin !== undefined;
}

function describe(error: unknown): string {
    return error instanceof Error ? error.message : String(error);
}
