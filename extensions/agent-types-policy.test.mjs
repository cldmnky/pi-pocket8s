// The decisions the agent-type extension makes without Pi: which project folders a trust answer opens, the thinking
// level a spawn asks for, and when a type's model gives way to the session's. The extension itself imports the Pi
// runtime, so these live in _agent-types-policy.ts and are tested here on their own.
import test from "node:test";
import assert from "node:assert/strict";
import {
    THINKING,
    canThinkAtOrBelow,
    fallsBackToSessionModel,
    projectFolders,
    wantedThinking,
} from "./_agent-types-policy.ts";

const glm = { model: { provider: "opencode-go", modelId: "glm-5.3" }, thinkingLevels: ["low", "high", "max"] };
const k3 = { model: { provider: "opencode-go", modelId: "kimi-k3" }, thinkingLevels: ["max"] };
const parentGlm = { provider: "opencode-go", modelId: "glm-5.3" };
const parentK3 = { provider: "opencode-go", modelId: "kimi-k3" };
const allowed = ["opencode-go/kimi-k3", "opencode-go/glm-5.3"];

test("the thinking levels are Pi's seven, lowest first", () => {
    assert.deepEqual(THINKING, ["off", "minimal", "low", "medium", "high", "xhigh", "max"]);
});

test("a spawn asks for the caller's level, capped at the session's", () => {
    assert.equal(wantedThinking(undefined, "high"), "high");
    assert.equal(wantedThinking("low", "high"), "low");
    assert.equal(wantedThinking("max", "high"), "high");
    assert.equal(wantedThinking("max", "max"), "max");
    assert.equal(wantedThinking("unknown", "high"), "unknown", "an unknown level is left to the built-in");
});

test("a model can think at or below a level only when it offers one", () => {
    assert.equal(canThinkAtOrBelow(["max"], "high"), false, "kimi-k3 offers only max");
    assert.equal(canThinkAtOrBelow(["max"], "max"), true);
    assert.equal(canThinkAtOrBelow(["low", "high", "max"], "off"), false);
    assert.equal(canThinkAtOrBelow(["low", "high", "max"], "high"), true);
    assert.equal(canThinkAtOrBelow(["low", "high", "max"], "medium"), true, "low is below medium");
});

test("project trust opens .pi/agents unless distrusted, .agents/agents only when trusted", () => {
    assert.deepEqual(projectFolders("/repo", "trusted"), [
        "/repo/.pi/agents",
        "/repo/.agents/agents",
    ]);
    assert.deepEqual(projectFolders("/repo", "undecided"), ["/repo/.pi/agents"]);
    assert.deepEqual(projectFolders("/repo", "distrusted"), []);
    assert.deepEqual(projectFolders(undefined, "trusted"), []);
});

test("a foreign model that cannot think at the session's level falls back", () => {
    const base = { typeModel: "opencode-go/kimi-k3", parentModel: parentGlm, info: k3, allowed, asked: "max", sessionLevel: "high" };

    assert.equal(fallsBackToSessionModel(base), true, "k3 cannot run at the session's high level");
    assert.equal(fallsBackToSessionModel({ ...base, sessionLevel: "max" }), false, "k3 runs at max");
    assert.equal(fallsBackToSessionModel({ ...base, parentModel: parentK3 }), false, "the session's own model is left to the built-in");
    assert.equal(fallsBackToSessionModel({ ...base, info: glm }), false, "glm can think at high");
    assert.equal(fallsBackToSessionModel({ ...base, allowed: [] }), false, "a disallowed model is the built-in's refusal");
    assert.equal(fallsBackToSessionModel({ ...base, info: undefined }), false, "an unknown model is the built-in's refusal");
    assert.equal(fallsBackToSessionModel({ ...base, parentModel: undefined }), false, "there is no session model to fall back to");
    assert.equal(fallsBackToSessionModel({ ...base, typeModel: undefined }), false);
});

test("a type's lower thinking level is honored when its model offers it", () => {
    assert.equal(
        fallsBackToSessionModel({
            typeModel: "opencode-go/glm-5.3",
            parentModel: parentK3,
            info: glm,
            allowed,
            asked: "low",
            sessionLevel: "max",
        }),
        false,
        "glm's low is below the session's max",
    );
});

// 0.20 changed what the allow-list means: while it is empty a subagent runs only on its session's model, and while it
// is nonempty, only on the models it lists — including the session's own. A fallback that drops a type's model for a
// session model the list does not have would therefore be refused, and with a less useful message than the type
// model's own refusal, so both models have to be allowed before the type's gives way.
test("a fallback needs the session's model on the allow-list too", () => {
    const options = {
        typeModel: "opencode-go/kimi-k3",
        parentModel: parentGlm,
        info: k3,
        allowed: ["opencode-go/kimi-k3"],
        asked: "max",
        sessionLevel: "high",
    };

    assert.equal(
        fallsBackToSessionModel(options),
        false,
        "the type model is allowed but the session's is not: the type's model is kept for the built-in to judge",
    );
    assert.equal(
        fallsBackToSessionModel({ ...options, allowed }),
        true,
        "both listed: the existing fallback applies",
    );
});
