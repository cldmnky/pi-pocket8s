/**
 * Model plumbing for the web_search tool: Pi's model runtime, and the small context shim the
 * vendored pi-web-search code expects.
 *
 * Pi Pocket has no "current model" for extensions to read (each conversation picks its own), so a
 * search runs on the model named in `~/.pi/agent/web-search.json`, or on the best available model
 * from `select.ts` when that file does not exist.
 */
import { ModelRegistry, ModelRuntime } from "@earendil-works/pi-coding-agent";
import type { ExtensionContext } from "@earendil-works/pi-coding-agent";
import type { Api, Model } from "@earendil-works/pi-ai";

let registry: Promise<ModelRegistry> | undefined;

/** One ModelRuntime for this module; it reads the install's own auth.json and settings. */
export function modelRegistry(): Promise<ModelRegistry> {
    registry ??= ModelRuntime.create().then((runtime) => new ModelRegistry(runtime));
    return registry;
}

/**
 * The slice of Pi's extension context the vendored code uses: the model registry, the model to
 * search with, and a session id — only OpenCode, which requires one per conversation, reads it.
 */
export function searchContext(
    models: ModelRegistry,
    model: Model<Api> | undefined,
    conversationId: string,
): ExtensionContext {
    return {
        model,
        modelRegistry: models,
        sessionManager: { getSessionId: () => conversationId },
    } as unknown as ExtensionContext;
}
