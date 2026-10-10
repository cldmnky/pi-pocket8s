// The subagent types the image ships as data (extensions/agent-types/). The module that reads them is
// exercised by the image build and the CI smoke job, which can import it; this test pins the files'
// contract without any dependency, the way extensions/web-search.test.mjs does for vendored code.
//
// The frontmatter rules mirrored here are the module's: a leading `---` block of `key: value` lines,
// unknown keys ignored (so a file written for `pi`'s subagents works), `tools` split on commas, and
// every file whose name starts with `_` skipped as a type.
import test from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const DIR = fileURLToPath(new URL("./agent-types/", import.meta.url));
const TOOLS = new Set(["read", "write", "edit", "bash", "artifact", "browser", "subagent", "schedule", "codemode", "web_search"]);
const LEVELS = ["off", "minimal", "low", "medium", "high", "xhigh", "max"];
/** The models the three roles are built on: the OpenCode Go subscription the workspace has. */
const MODELS = { architect: "opencode-go/kimi-k3", coder: "opencode-go/deepseek-v4.1-flash", reviewer: "opencode-go/glm-5.3" };

function parse(text) {
    const lines = text.split(/\r?\n/);
    assert.equal(lines[0]?.trim(), "---", "a type file starts with a frontmatter block");
    const end = lines.findIndex((line, index) => index > 0 && line.trim() === "---");
    assert.notEqual(end, -1, "the frontmatter block is closed");
    const fields = {};
    for (const line of lines.slice(1, end)) {
        const field = /^([A-Za-z][A-Za-z0-9_-]*):\s*(.*)$/.exec(line);
        if (field !== null) fields[field[1].toLowerCase()] = field[2].trim();
    }
    return { fields, body: lines.slice(end + 1).join("\n").trim() };
}

const files = readdirSync(DIR).filter((file) => file.endsWith(".md") && !file.startsWith("_"));
const types = Object.fromEntries(files.map((file) => [file.slice(0, -3), parse(readFileSync(join(DIR, file), "utf8"))]));

test("the shipped types are the three roles, with the documented models and levels", () => {
    assert.deepEqual(files.sort(), ["architect.md", "coder.md", "reviewer.md"]);

    for (const [name, model] of Object.entries(MODELS)) {
        const { fields } = types[name];
        assert.equal(fields.model, model, `${name} runs on the model the workflow assigns it`);
        assert.ok(LEVELS.includes(fields.thinking), `${name}: thinking is one of the six levels`);
        assert.ok(fields.description.length > 20, `${name}: has a description for the agent to pick it by`);
    }
});

test("each type is usable: known tools, a brief, and read-only roles that stay read-only", () => {
    for (const [name, { fields, body }] of Object.entries(types)) {
        const tools = (fields.tools ?? "").split(",").map((tool) => tool.trim()).filter(Boolean);

        assert.ok(tools.length > 0, `${name}: names its tools`);
        for (const tool of tools) assert.ok(TOOLS.has(tool), `${name}: ${tool} is a real tool name`);
        assert.ok(body.length > 200, `${name}: carries a brief that says what to do`);
        assert.match(body, /reported back/, `${name}: tells the subagent its answer is the report`);

        // The read-only guarantee the docs make: it is the tool list, not the prompt, that enforces it.
        if (name === "architect" || name === "reviewer") {
            assert.ok(!tools.includes("write") && !tools.includes("edit"), `${name} must not be able to write`);
            assert.match(body, /change nothing|never implement or modify/i, `${name}: says so in the brief too`);
        }
    }

    assert.match(types.coder.body, /never claim a test passed/i, "the coder must not claim unverified test results");
    assert.match(types.reviewer.body, /severity/i, "the reviewer reports findings by severity");
});

test("the folder's readme documents the format and the workflow, and is not itself a type", () => {
    const readme = readFileSync(join(DIR, "_readme.md"), "utf8");

    assert.ok(!files.includes("_readme.md"), "files starting with _ are skipped as types");
    for (const token of ["enabled: false", "thinking", "model:", "tools:", "worktree", "architect"]) {
        assert.ok(readme.includes(token), `_readme.md mentions ${token}`);
    }
});
