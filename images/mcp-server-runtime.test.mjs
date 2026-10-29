// The posture of the Kubernetes MCP server the image ships, taken from the server itself rather than from the
// configuration it was given: start it over stdio with the shipped TOML, ask it for its tool list, and look at what
// it would offer the agent. A configuration that says `read_only = true` is a claim; the tools it registers are the
// proof.
//
// The binary and its configuration are named, not guessed: `MCP_SERVER_BIN` and `MCP_SERVER_CONFIG`. The image build
// sets both (to the installed binary and `/usr/share/pi-agent/mcp/kubernetes-read-only.toml`). Unset, every test here
// skips, which is what `make test` in a checkout does — the file to download is ~87 MB, and a checkout has no server.
//
// The shipped TOML points the server at the pod's kubeconfig, which a checkout does not have. A `--config-dir` drop-in
// in a temporary directory overrides just that one key with a stub kubeconfig that names an unreachable API server:
// listing tools needs no cluster, and a request that did reach one would fail on the stub rather than touch a real
// cluster.
import test from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

/** The pinned server, and the TOML the image ships. Both are set by the image build. */
const BIN = process.env.MCP_SERVER_BIN;
const CONFIG = process.env.MCP_SERVER_CONFIG;

const skip =
    !BIN
        ? "MCP_SERVER_BIN is not set (the image build points it at the installed server)"
        : !CONFIG
          ? "MCP_SERVER_CONFIG is not set (the image build points it at the shipped TOML)"
          : !existsSync(BIN)
            ? `no Kubernetes MCP server at ${BIN}`
            : !existsSync(CONFIG)
              ? `no configuration at ${CONFIG}`
              : false;

/** Tools the read-only server must still offer: the everyday reads an agent needs to look at a cluster. */
const ALLOWED = ["events_list", "pods_get", "pods_list", "pods_list_in_namespace", "pods_log", "resources_get", "resources_list"];

/**
 * A write tool must not be registered at all, so calling it by name has to fail as an unknown tool. The pattern is
 * deliberately broad: a version bump that adds a write tool of any shape fails here, not in a cluster.
 */
const DENIED = /create|apply|patch|delete|exec|run|scale|helm_/;
/** The kubeconfig reader: off with the config toolset, and never part of a read-only cluster posture. */
const CONFIGURATION_VIEW = "configuration_view";
/** One name per shape the denylist covers, called below to prove the tool really is not there. */
const WRITE_TOOLS = ["pods_run", CONFIGURATION_VIEW];

/** A kubeconfig whose API server refuses every connection: enough for discovery-free tool listing, unusable for a cluster. */
const STUB_KUBECONFIG = `# Written by images/mcp-server-runtime.test.mjs: no cluster is reachable through this.
apiVersion: v1
kind: Config
clusters:
    - name: stub
      cluster:
          server: https://127.0.0.1:1
users:
    - name: stub
      user:
          token: stub
contexts:
    - name: stub
      context:
          cluster: stub
          user: stub
          namespace: default
current-context: stub
preferences: {}
`;

/** JSON-RPC over stdin/stdout, one JSON object per line. */
function session(requests, timeoutMs = 30000) {
    const directory = mkdtempSync(join(tmpdir(), "mcp-server-"));
    const kubeconfig = join(directory, "kubeconfig");
    writeFileSync(kubeconfig, STUB_KUBECONFIG);
    writeFileSync(join(directory, "10-kubeconfig.toml"), `kubeconfig = ${JSON.stringify(kubeconfig)}\n`);

    const child = spawn(BIN, ["--config", CONFIG, "--config-dir", directory], {
        stdio: ["pipe", "pipe", "pipe"],
        env: { ...process.env, KUBECONFIG: kubeconfig },
    });

    let stdout = "";
    let stderr = "";
    const seen = new Map();
    const pending = new Set(requests.map((request) => request.id));
    const diagnostics = () => `server at ${BIN}\nstdout:\n${stdout}\nstderr:\n${stderr}`;

    return new Promise((resolve, reject) => {
        let settled = false;
        const finish = (error) => {
            if (settled) {
                return;
            }
            settled = true;
            clearTimeout(timer);
            child.kill("SIGKILL");
            rmSync(directory, { recursive: true, force: true });
            if (error) {
                reject(error);
            } else {
                resolve(seen);
            }
        };

        const timer = setTimeout(() => finish(new Error(`no answer from the server within ${timeoutMs} ms\n${diagnostics()}`)), timeoutMs);

        child.on("error", (error) => finish(new Error(`cannot start ${BIN}: ${error.message}`)));
        // A server that exits before answering (a configuration it refuses, a missing kubeconfig) is reported
        // with its own stderr, not as a wait that times out.
        child.on("close", (code) => {
            if (pending.size > 0) {
                finish(new Error(`the server exited with code ${code} before answering ${[...pending].join(", ")}\n${diagnostics()}`));
            }
        });

        child.stdout.on("data", (chunk) => {
            stdout += String(chunk);
            const lines = stdout.split("\n");
            stdout = lines.pop() ?? "";
            for (const line of lines) {
                if (line.trim() === "") {
                    continue;
                }
                let message;
                try {
                    message = JSON.parse(line);
                } catch {
                    finish(new Error(`the server wrote a line that is not JSON-RPC: ${line}\n${diagnostics()}`));
                    return;
                }
                if (message.id === undefined || !pending.has(message.id)) {
                    continue;
                }
                pending.delete(message.id);
                seen.set(message.id, message);
                if (pending.size === 0) {
                    finish();
                    return;
                }
            }
        });

        child.stderr.on("data", (chunk) => {
            stderr += String(chunk);
        });

        child.stdin.on("error", () => {
            // The server exited before it read everything; the close handler above reports why.
        });

        for (const request of requests) {
            child.stdin.write(`${JSON.stringify(request)}\n`);
        }
    });
}

const INITIALIZE = {
    jsonrpc: "2.0",
    id: 1,
    method: "initialize",
    params: {
        protocolVersion: "2025-06-18",
        capabilities: {},
        clientInfo: { name: "images/mcp-server-runtime.test.mjs", version: "1" },
    },
};

/** One list request and one call per write-tool name; the session answers all of them. */
const REQUESTS = [
    INITIALIZE,
    { jsonrpc: "2.0", method: "notifications/initialized" },
    { jsonrpc: "2.0", id: 2, method: "tools/list", params: {} },
    ...WRITE_TOOLS.map((name, index) => ({
        jsonrpc: "2.0",
        id: 3 + index,
        method: "tools/call",
        params: { name, arguments: { namespace: "default", name: "smoke", command: ["true"] } },
    })),
];

/** The one session every test reads, so the server is started once. */
let running;
function answers() {
    running ??= session(REQUESTS.filter((request) => request.id !== undefined));
    return running;
}

test.after(async () => {
    await running?.catch(() => {});
});

test("the shipped configuration starts the pinned server", { skip }, async () => {
    const seen = await answers();
    const initialized = seen.get(1);

    assert.equal(initialized?.error, undefined, `initialize failed: ${JSON.stringify(initialized)}`);
    assert.equal(initialized.result.serverInfo.name, "kubernetes-mcp-server");
    assert.equal(typeof initialized.result.serverInfo.version, "string");
});

test("every tool the server offers is read-only", { skip }, async () => {
    const seen = await answers();
    const reply = seen.get(2);
    assert.equal(reply?.error, undefined, `tools/list failed: ${JSON.stringify(reply)}`);

    const tools = reply.result.tools;
    assert.ok(Array.isArray(tools) && tools.length > 0, `the server offered no tools: ${JSON.stringify(reply)}`);

    const writable = tools.filter((tool) => tool.annotations?.readOnlyHint !== true).map((tool) => tool.name);
    assert.deepEqual(
        writable,
        [],
        `these tools are not annotated read-only, so read_only = true should have hidden them: ${JSON.stringify(tools.map((tool) => [tool.name, tool.annotations]))}`,
    );
});

test("no write tool and no kubeconfig reader is registered", { skip }, async () => {
    const seen = await answers();
    const tools = seen.get(2).result.tools;

    const names = tools.map((tool) => tool.name).sort();
    const denied = names.filter((name) => DENIED.test(name) || name === CONFIGURATION_VIEW);
    assert.deepEqual(denied, [], `write tools reached the agent. Observed tools: ${JSON.stringify(names)}`);

    const missing = ALLOWED.filter((name) => !names.includes(name));
    assert.deepEqual(missing, [], `read tools an agent needs are missing. Observed tools: ${JSON.stringify(names)}`);
});

test("a write tool cannot be called by name", { skip }, async () => {
    const seen = await answers();

    for (const [index, name] of WRITE_TOOLS.entries()) {
        const reply = seen.get(3 + index);
        assert.equal(reply?.error?.code, -32602, `${name} is not rejected as an unknown tool: ${JSON.stringify(reply)}`);
        assert.match(reply.error.message, /unknown tool/, `${name} is not rejected as an unknown tool: ${reply.error.message}`);
    }
});
