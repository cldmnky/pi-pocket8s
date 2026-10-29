// The project-trust wiring and the model fallback, exercised against the real extension and a fake built-in. It needs
// Pi's packages, so it skips where they are not installed (the repository's `make test` outside the image); the
// Containerfile runs it with AGENT_TYPES_MODULE pointing at the module it copied into the image.
import test from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, realpath, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const moduleUrl =
    process.env.AGENT_TYPES_MODULE ?? new URL("./agent-types.ts", import.meta.url).href;
const unavailable = "Pi runtime not installed";

let createAgentTypes;
try {
    ({ default: createAgentTypes } = await import(moduleUrl));
} catch {
    // The repository test runner has no Pi packages; the image build does.
}

/** The built-in's answer: Pi Durable 1.4 tool results carry their parts in `output`. */
const nativeOutput = { output: [{ type: "text", text: "ok" }] };

const fakeBuiltIn = {
    name: "subagent",
    description: "built-in",
    parameters: {
        type: "object",
        properties: {
            action: { anyOf: [{ type: "string", const: "spawn" }] },
            name: { type: "string" },
            message: { type: "string" },
            thinking: { type: "string" },
            tools: { type: "array", items: { type: "string" } },
            model: { type: "string" },
        },
    },
    calls: [],
    async execute(args) {
        this.calls.push(args);
        return nativeOutput;
    },
};

async function setup() {
    const base = await mkdtemp(join(tmpdir(), "agent-types-trust-"));
    const cwd = join(base, "repo");
    const agentDir = join(base, "agent");
    await mkdir(join(cwd, ".pi", "agents"), { recursive: true });
    await mkdir(join(cwd, ".agents", "agents"), { recursive: true });
    await mkdir(join(agentDir, "agents"), { recursive: true });
    await writeFile(join(cwd, ".pi", "agents", "local.md"), "---\nmodel: opencode-go/glm-5.3\n---\nlocal");
    await writeFile(join(cwd, ".agents", "agents", "team.md"), "---\nmodel: opencode-go/glm-5.3\n---\nteam");
    // A type whose model offers only `max`, so a session at `high` is where the fallback question is asked.
    await writeFile(
        join(agentDir, "agents", "deep.md"),
        "---\nmodel: opencode-go/kimi-k3\nthinking: max\ntools: read, bash\ndescription: deep work\n---\nDeep brief.",
    );
    await writeFile(join(agentDir, "agents", "unknown.md"), "---\nmodel: vendor/not-here\n---\nUnknown brief.");

    return { base, cwd, agentDir };
}

const info = {
    "opencode-go/glm-5.3": {
        model: { provider: "opencode-go", modelId: "glm-5.3" },
        name: "GLM",
        thinkingLevels: ["low", "high", "max"],
    },
    "opencode-go/kimi-k3": {
        model: { provider: "opencode-go", modelId: "kimi-k3" },
        name: "Kimi K3",
        thinkingLevels: ["max"],
    },
};

function hostFor(agentDir, options = {}) {
    return {
        notice() {},
        agentDir,
        modelInfo(spec) {
            return info[spec];
        },
        subagentModels() {
            return options.allowed ?? ["opencode-go/glm-5.3"];
        },
    };
}

/** Spawn through the wrapped tool and report what the built-in was given, and what it answered. */
async function spawn(cwd, agentDir, options = {}) {
    const extension = createAgentTypes(hostFor(agentDir, options));
    const wrap = extension.wraps.find((each) => each.tool === "subagent");
    const wrapped = wrap.wrap(fakeBuiltIn);
    const args = {
        action: "spawn",
        name: "check",
        message: "go",
        ...(options.type === undefined ? {} : { type: options.type }),
        ...(options.args ?? {}),
    };
    const parent = {
        cwd,
        model: options.parentModel ?? { provider: "opencode-go", modelId: "glm-5.3" },
        thinkingLevel: options.parentLevel ?? "high",
    };

    fakeBuiltIn.calls.length = 0;

    const result = await wrapped.execute(args, { agent: async () => parent }, {});

    return { calls: [...fakeBuiltIn.calls], result };
}

async function canSpawn(cwd, agentDir, type) {
    try {
        return (await spawn(cwd, agentDir, { type })).calls.length === 1;
    } catch {
        return false;
    }
}

test("defaultProjectTrust comes from global settings, not the project", { skip: createAgentTypes === undefined && unavailable }, async () => {
    const { base, cwd, agentDir } = await setup();

    try {
        await writeFile(join(agentDir, "settings.json"), JSON.stringify({ defaultProjectTrust: "always" }));
        await writeFile(join(cwd, ".pi", "settings.json"), JSON.stringify({ defaultProjectTrust: "never" }));

        assert.equal(await canSpawn(cwd, agentDir, "local"), true, ".pi/agents loads with global always");
        assert.equal(await canSpawn(cwd, agentDir, "team"), true, ".agents/agents loads with global always; the project's never is ignored");
    } finally {
        await rm(base, { recursive: true, force: true });
    }
});

test("a corrupt trust store decides nothing, even with defaultProjectTrust always", { skip: createAgentTypes === undefined && unavailable }, async () => {
    const { base, cwd, agentDir } = await setup();

    try {
        await writeFile(join(agentDir, "settings.json"), JSON.stringify({ defaultProjectTrust: "always" }));
        await writeFile(join(agentDir, "trust.json"), "{ not json");

        assert.equal(await canSpawn(cwd, agentDir, "local"), true, "an unreadable store still loads .pi/agents");
        assert.equal(await canSpawn(cwd, agentDir, "team"), false, "an unreadable store must not load .agents/agents");
    } finally {
        await rm(base, { recursive: true, force: true });
    }
});

test("a saved decision wins over the global default", { skip: createAgentTypes === undefined && unavailable }, async () => {
    const { base, cwd, agentDir } = await setup();

    try {
        await writeFile(join(agentDir, "settings.json"), JSON.stringify({ defaultProjectTrust: "always" }));
        const key = await realpath(cwd);
        await writeFile(join(agentDir, "trust.json"), JSON.stringify({ [key]: false }));

        assert.equal(await canSpawn(cwd, agentDir, "local"), false, "a saved no excludes .pi/agents");
        assert.equal(await canSpawn(cwd, agentDir, "team"), false, "a saved no excludes .agents/agents");

        await writeFile(join(agentDir, "settings.json"), JSON.stringify({ defaultProjectTrust: "never" }));
        await writeFile(join(agentDir, "trust.json"), JSON.stringify({ [key]: true }));

        assert.equal(await canSpawn(cwd, agentDir, "local"), true, "a saved yes includes .pi/agents");
        assert.equal(await canSpawn(cwd, agentDir, "team"), true, "a saved yes includes .agents/agents");
    } finally {
        await rm(base, { recursive: true, force: true });
    }
});

test("a spawn without a type is the built-in's own call", { skip: createAgentTypes === undefined && unavailable }, async () => {
    const { base, cwd, agentDir } = await setup();

    try {
        const args = { model: "opencode-go/glm-5.3", thinking: "low", tools: ["read"] };
        const { calls, result } = await spawn(cwd, agentDir, { args });

        assert.equal(calls.length, 1);
        assert.deepEqual(calls[0], { action: "spawn", name: "check", message: "go", ...args }, "the arguments pass through as given");
        assert.deepEqual(result, nativeOutput, "the built-in's output is returned as it came");
        assert.equal(result.content, undefined, "API 2 results carry output, not content");
    } finally {
        await rm(base, { recursive: true, force: true });
    }
});

test("both models allowed and the type's cannot think that low: the type's model gives way, its brief, thinking and tools stay", { skip: createAgentTypes === undefined && unavailable }, async () => {
    const { base, cwd, agentDir } = await setup();

    try {
        const { calls, result } = await spawn(cwd, agentDir, {
            type: "deep",
            allowed: ["opencode-go/glm-5.3", "opencode-go/kimi-k3"],
        });
        const call = calls[0];

        assert.equal("model" in call, false, "the type's model is dropped for the session's");
        assert.equal(call.thinking, "max", "the type's thinking level still applies");
        assert.deepEqual(call.tools, ["read", "bash"], "the type's tool list still applies");
        assert.match(call.message, /^Deep brief\.\n\ngo$/, "the type's brief is prepended to the caller's message");
        assert.deepEqual(result, nativeOutput);
    } finally {
        await rm(base, { recursive: true, force: true });
    }
});

test("a session model the allow-list does not have keeps the allowed type model", { skip: createAgentTypes === undefined && unavailable }, async () => {
    const { base, cwd, agentDir } = await setup();

    try {
        const { calls, result } = await spawn(cwd, agentDir, {
            type: "deep",
            allowed: ["opencode-go/kimi-k3"],
        });

        assert.equal(calls[0].model, "opencode-go/kimi-k3", "dropping it would leave the built-in with no usable model");
        assert.equal(calls[0].thinking, "max");
        assert.deepEqual(calls[0].tools, ["read", "bash"]);
        assert.deepEqual(result, nativeOutput);
    } finally {
        await rm(base, { recursive: true, force: true });
    }
});

test("an unknown or disallowed type model stays the built-in's refusal, with no substitution", { skip: createAgentTypes === undefined && unavailable }, async () => {
    const { base, cwd, agentDir } = await setup();

    try {
        const unknown = await spawn(cwd, agentDir, {
            type: "unknown",
            allowed: ["opencode-go/glm-5.3"],
        });

        assert.equal(unknown.calls[0].model, "vendor/not-here", "an unknown model is left to the built-in to refuse");
        assert.deepEqual(unknown.result, nativeOutput, "the refusal is the built-in's answer, not a switch to another model");

        const disallowed = await spawn(cwd, agentDir, {
            type: "local",
            allowed: ["opencode-go/kimi-k3"],
            parentModel: { provider: "opencode-go", modelId: "kimi-k3" },
        });

        assert.equal(disallowed.calls[0].model, "opencode-go/glm-5.3", "a disallowed type model is refused, not replaced");
        assert.deepEqual(disallowed.result, nativeOutput);
    } finally {
        await rm(base, { recursive: true, force: true });
    }
});
