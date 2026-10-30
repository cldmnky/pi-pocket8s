// The Kubernetes MCP server's shipped configuration, checked in the checkout: no binary and no network
// needed, so `make test` covers it on a laptop as well as in CI. What the server itself does with this
// configuration is images/mcp-server-runtime.test.mjs; this file is about the two files and the paths they name.
//
// The entry is inert. Nothing in the image installs the gallery "mcp" extension, so ~/.pi/agent/mcp.json starts no
// server until the owner installs that extension and switches "kubernetes" on.
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import test from "node:test";

/** Where the image puts the shipped Pi agent configuration; the entrypoint seeds from here. */
const SHIPPED = "/usr/share/pi-agent";
/** The entry as it must be shipped, key for key: anything else changes what the owner switches on. */
const ENTRY = {
    command: "/usr/local/bin/kubernetes-mcp-server",
    args: ["--config", `${SHIPPED}/mcp/kubernetes-read-only.toml`],
    description:
        "Read-only Kubernetes and OpenShift cluster access as this workspace's service account: list and get pods, resources, and events, and read pod logs.",
};

const manifest = JSON.parse(readFileSync("pi-agent/mcp.json", "utf8"));
const toml = readFileSync("pi-agent/mcp/kubernetes-read-only.toml", "utf8");
const containerfile = readFileSync("images/Containerfile", "utf8");
const entrypoint = readFileSync("images/entrypoint.sh", "utf8");

test("mcp.json declares exactly the one read-only kubernetes server", () => {
    assert.deepEqual(Object.keys(manifest), ["mcpServers"], "mcp.json is { mcpServers: { … } } and nothing else");
    assert.deepEqual(Object.keys(manifest.mcpServers), ["kubernetes"], "the one entry is named kubernetes");
    assert.deepEqual(
        manifest.mcpServers.kubernetes,
        ENTRY,
        "the kubernetes entry is the reviewed one: the installed binary, the shipped TOML, the read-only description",
    );
});

test("the TOML the entry names is a real shipped file", () => {
    const [flag, path] = manifest.mcpServers.kubernetes.args;

    assert.equal(flag, "--config", "the server is started with --config, not a drop-in directory or stdin");
    assert.ok(path.startsWith(`${SHIPPED}/`), `the configuration is shipped in the image, not in the workspace: ${path}`);
    assert.ok(existsSync(`pi-agent/${path.slice(SHIPPED.length + 1)}`), `nothing in pi-agent/ becomes ${path}`);
    assert.ok(existsSync("pi-agent/mcp/kubernetes-read-only.toml"), "pi-agent/mcp/kubernetes-read-only.toml is the file");
});

test("the TOML posture is read-only, core toolsets, and no Secrets", () => {
    assert.match(toml, /^read_only = true$/m, "read_only = true hides every tool that is not annotated read-only");
    assert.match(toml, /^toolsets = \["core"\]$/m, "the core toolset only: no helm, no config, no kubevirt");
    assert.match(toml, /^log_level = 2$/m, "log_level is a known key; an unknown one fails the load");
    assert.match(
        toml,
        /^kubeconfig = "\/workspace\/home\/\.kube\/config"$/m,
        "the kubeconfig the entrypoint writes for the pod's service account",
    );

    // One table after another, so the denial is read as a block and not as three lines that happen to exist.
    const tables = toml
        .split(/^\[\[/m)
        .slice(1)
        .map((table) => table.split("\n").map((line) => line.trim()));
    const denied = tables.filter((table) => table[0] === "denied_resources]]");
    assert.equal(denied.length, 1, "the shipped TOML denies exactly one resource type");
    assert.deepEqual(
        denied[0].slice(1).filter((line) => line !== ""),
        ["group = \"\"", "version = \"v1\"", "kind = \"Secret\""],
        "the denial is a core v1 Secret: its contents must stay out of the model's reach",
    );
});

test("the image installs the binary the entry names, and ships pi-agent/ where the entry expects it", () => {
    assert.ok(/^COPY pi-agent\/ \/usr\/share\/pi-agent\/$/m.test(containerfile), "pi-agent/ does not land at /usr/share/pi-agent");
    assert.ok(
        /install -m 0755 "\$\{asset\}" \/usr\/local\/bin\/kubernetes-mcp-server/.test(containerfile),
        "the pinned download is not installed at the path mcp.json names",
    );
    assert.ok(
        /^ARG KUBERNETES_MCP_SERVER_VERSION=v0\.1\.1$/m.test(containerfile),
        "the server version is not pinned as v0.1.1",
    );
});

test("the entrypoint seeds mcp.json into the workspace home, copy-if-missing", () => {
    assert.ok(
        /^\s*"mcp\.json:mcp\.json"(?: \\|; do)$/m.test(entrypoint),
        "seed_pi_agent_files does not carry mcp.json: a fresh workspace would have no entry to switch on",
    );
});
