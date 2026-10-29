// What the image's copy of upstream Pi Pocket must be, checked where that copy lives.
//
// It is the part of the 0.26.0 pin a grep of the Containerfile cannot prove: the checkout's own version, the Pi
// packages the shipped extensions are written against, the extension API level the application loads, and that the
// shipped models.json actually turns kimi-k3's strict-mode flag off once Pi has merged it.
//
// `POCKET_APP_DIR` names that copy, and the image build sets it to `/opt/pi-pocket`. It is required rather than
// defaulted: a workstation can have another Pi Pocket there (this repository's own development pod has the running
// app), and an assertion about the image's copy must not silently become an assertion about that one. Unset, every
// test here skips, which is what `make test` in a checkout does.
import test from "node:test";
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

/** The application as the image installs it: `/opt/pi-pocket`, named by `POCKET_APP_DIR`. */
const APP = process.env.POCKET_APP_DIR;
/** The version the Containerfile pins (`PI_POCKET_VERSION`); a bump changes both. */
const VERSION = "0.26.0";
/** The Pi packages the extensions import; 0.26.0 ships 1.4.0 of each as a direct dependency. */
const PACKAGES = [
    "@earendil-works/chord",
    "@earendil-works/pi-ai",
    "@earendil-works/pi-codemode",
    "@earendil-works/pi-coding-agent",
    "@earendil-works/pi-durable",
    "@earendil-works/pi-mcp",
];

/** The image build only: it names the folder. */
const skip =
    APP === undefined
        ? "POCKET_APP_DIR is not set (the image build sets it)"
        : !existsSync(join(APP, "package.json"))
          ? `no application at ${APP}`
          : false;

function manifest(path) {
    return JSON.parse(readFileSync(path, "utf8"));
}

/**
 * Import a package from the application's own node_modules. A bare specifier would resolve from this file's folder
 * instead, and the package exports only its `import` target, so its manifest says which file that is.
 */
async function fromApp(name) {
    const directory = join(APP, "node_modules", name);
    const entry = manifest(join(directory, "package.json")).exports?.["."]?.import ?? manifest(join(directory, "package.json")).main;

    return import(pathToFileURL(join(directory, entry)).href);
}

/** Where the seeded models.json is: the image seeds it into the workspace home before the application starts. */
function modelsPath() {
    return process.env.POCKET_MODELS_JSON ?? join(process.env.PI_CODING_AGENT_DIR ?? join(homedir(), ".pi", "agent"), "models.json");
}

test("the checkout is the pinned 0.26.0 release", { skip }, () => {
    assert.equal(manifest(join(APP, "package.json")).version, VERSION);
});

test("the Pi packages are the 1.4.0 the shipped extensions are written against", { skip }, () => {
    const dependencies = manifest(join(APP, "package.json")).dependencies ?? {};

    for (const name of PACKAGES) {
        assert.equal(dependencies[name], "1.4.0", `${name} must be a direct dependency at 1.4.0`);
        assert.equal(
            manifest(join(APP, "node_modules", name, "package.json")).version,
            "1.4.0",
            `${name} installed at another version than the lockfile's`,
        );
    }
});

test("the application loads extension API 1 to 2", { skip }, () => {
    const source = readFileSync(join(APP, "src", "server", "extension-web.ts"), "utf8");

    assert.match(source, /export const POCKET_API = 2;/, "API 2 is what the shipped extensions declare");
    assert.match(source, /export const OLDEST_POCKET_API = 1;/, "API 1 still loads, so an old drop-in is not refused");
});

test("the shipped models.json turns kimi-k3's strict mode off, as Pi resolves it", { skip }, async () => {
    const { ModelRuntime } = await fromApp("@earendil-works/pi-coding-agent");
    const path = modelsPath();

    assert.ok(existsSync(path), `the models.json the app reads must exist: ${path}`);

    const withFix = await ModelRuntime.create({ modelsPath: path, refreshOnCreate: false });
    const model = withFix.getModel("opencode-go", "kimi-k3");

    assert.ok(model !== undefined, "opencode-go/kimi-k3 must be in Pi's catalog");
    assert.equal(model.compat?.supportsStrictMode, false, "the override is what the gateway needs");

    // The catalog itself still claims strict mode: that is what makes the override necessary, and what will make it
    // removable once upstream changes.
    const withoutFix = await ModelRuntime.create({ modelsPath: null, refreshOnCreate: false });

    assert.equal(
        withoutFix.getModel("opencode-go", "kimi-k3")?.compat?.supportsStrictMode,
        true,
        "the catalog no longer advertises strict mode, so the override can go",
    );
});
