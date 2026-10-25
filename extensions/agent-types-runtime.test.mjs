// The project-trust wiring, exercised against the real extension and a fake built-in. It needs Pi's packages, so it
// skips where they are not installed (the repository's `make test` outside the image); the Containerfile runs it with
// AGENT_TYPES_MODULE pointing at the module it copied into the image.
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
        return { content: [{ type: "text", text: "ok" }] };
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

    return { base, cwd, agentDir };
}

function hostFor(agentDir) {
    return {
        notice() {},
        agentDir,
        modelInfo(spec) {
            return spec === "opencode-go/glm-5.3"
                ? {
                      model: { provider: "opencode-go", modelId: "glm-5.3" },
                      name: "GLM",
                      thinkingLevels: ["low", "high", "max"],
                  }
                : undefined;
        },
        subagentModels() {
            return ["opencode-go/glm-5.3"];
        },
    };
}

async function canSpawn(cwd, agentDir, type) {
    const extension = createAgentTypes(hostFor(agentDir));
    const wrap = extension.wraps.find((each) => each.tool === "subagent");
    const wrapped = wrap.wrap(fakeBuiltIn);

    fakeBuiltIn.calls.length = 0;

    try {
        await wrapped.execute(
            { action: "spawn", name: "check", message: "go", type },
            { agent: async () => ({ cwd, model: { provider: "opencode-go", modelId: "glm-5.3" }, thinkingLevel: "high" }) },
            {},
        );

        return true;
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
