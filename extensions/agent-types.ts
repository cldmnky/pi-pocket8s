/**
 * Preconfigured subagent types: markdown files with a model, thinking level, tool list, and brief, applied when the
 * built-in subagent tool spawns with `type`. Files come from `.pi/agents/` (unless the project is distrusted) or
 * `.agents/agents/` (only when it is trusted) in the session folder, or `~/.pi/agent/agents/` everywhere (the first
 * match wins). A type's model gives way to the session's model when it cannot think at the session's level; the
 * built-in still owns the model allow-list, the tool list, and the thinking cap.
 */
import { readdir, readFile } from "node:fs/promises";
import { join } from "node:path";
import { Type } from "@earendil-works/pi-ai";
import {
    defineExtension,
    defineTool,
    wrapSection,
    wrapTool,
    type PromptInput,
    type ToolExecutionApi,
    type ToolRegistration,
} from "@earendil-works/pi-durable";
import { ProjectTrustStore, SettingsManager } from "@earendil-works/pi-coding-agent";
import type { PocketHost } from "/opt/pi-pocket/src/server/host.ts";
import {
    THINKING,
    fallsBackToSessionModel,
    projectFolders,
    type ProjectTrust,
} from "./_agent-types-policy.ts";

type ToolContext = Parameters<ToolRegistration["execute"]>[2];

/** What the built-in subagent tool accepts (its own schema, minus the `type` this extension adds). */
type SubagentArgs = {
    action: "spawn" | "send" | "stop" | "status";
    name?: string;
    message?: string;
    followUp?: boolean;
    model?: string;
    thinking?: string;
    tools?: string[];
    type?: string;
};

/** One `.md` agent type file, parsed. */
type AgentType = {
    model?: string;
    thinking?: string;
    tools?: string[];
    description?: string;
    brief?: string;
};

/** A leading `---` block of `key: value` lines; unknown keys are ignored so `pi`'s agent files work here too. */
function parseFrontmatter(text: string): { fields: Record<string, string>; body: string } {
    const lines = text.split(/\r?\n/);

    if (lines[0]?.trim() !== "---") {
        return { fields: {}, body: text.trim() };
    }

    const end = lines.findIndex((line, index) => index > 0 && line.trim() === "---");
    const fields: Record<string, string> = {};

    for (const line of end === -1 ? lines.slice(1) : lines.slice(1, end)) {
        const field = /^([A-Za-z][A-Za-z0-9_-]*):\s*(.*)$/.exec(line);

        if (field !== null) {
            fields[field[1].toLowerCase()] = field[2].trim();
        }
    }

    return { fields, body: (end === -1 ? [] : lines.slice(end + 1)).join("\n").trim() };
}

/**
 * Pi's answer for a project, read as `skills.ts` reads it: a saved trust decision, else Pi's global
 * `defaultProjectTrust`. An unreadable store or settings file decides nothing, so only `.pi/agents` loads.
 */
function projectTrust(cwd: string, agentDir: string): ProjectTrust {
    let saved: boolean | null = null;

    try {
        saved = new ProjectTrustStore(agentDir).get(cwd);
    } catch {
        // A corrupt or unreadable trust store decides nothing: load only `.pi/agents`, as the skills do.
        return "undecided";
    }

    if (saved === true) {
        return "trusted";
    }

    if (saved === false) {
        return "distrusted";
    }

    let fallback = "ask";

    try {
        fallback = SettingsManager.create(cwd, agentDir).getGlobalSettings().defaultProjectTrust ?? "ask";
    } catch {
        // Pi's global settings unreadable: ask, so only `.pi/agents` loads.
    }

    return fallback === "always" ? "trusted" : fallback === "never" ? "distrusted" : "undecided";
}

/** Every agent type in scope for a session folder, by name; a project file shadows a global one with the same name. */
async function loadTypes(cwd: string | undefined, agentDir: string): Promise<Map<string, AgentType>> {
    const folders =
        cwd === undefined ? [] : projectFolders(cwd, projectTrust(cwd, agentDir));
    const types = new Map<string, AgentType>();
    const disabled = new Set<string>();

    folders.push(join(agentDir, "agents"));

    for (const folder of folders) {
        let files: string[];

        try {
            files = (await readdir(folder)).sort();
        } catch {
            continue;
        }

        for (const file of files) {
            if (!file.endsWith(".md") || file.startsWith("_")) {
                continue;
            }

            const name = file.slice(0, -3).toLowerCase();

            if (disabled.has(name) || types.has(name)) {
                continue;
            }

            const { fields, body } = parseFrontmatter(await readFile(join(folder, file), "utf8"));

            if (fields.enabled === "false") {
                disabled.add(name);
                continue;
            }

            types.set(name, {
                ...(fields.model === undefined || fields.model === "" ? {} : { model: fields.model }),
                ...(fields.thinking === undefined ? {} : { thinking: fields.thinking }),
                ...(fields.tools === undefined
                    ? {}
                    : { tools: fields.tools.split(",").map((tool) => tool.trim()).filter(Boolean) }),
                ...(fields.description === undefined ? {} : { description: fields.description }),
                ...(body === "" ? {} : { brief: body }),
            });
        }
    }

    return types;
}

export default function createAgentTypes(host: PocketHost) {
    // Only a name for wrapTool to target: never installed as a tool, so it takes nothing from the built-in one.
    const marker = defineTool({
        name: "subagent",
        description: "Marker for the built-in subagent tool.",
        parameters: Type.Object({}),
        execute: async () => ({ content: [] }),
    });

    const subagents = wrapTool(marker, (tool) => {
        // The built-in tool, behind the wrapper: called with the type applied, never bypassed.
        const builtIn = tool as unknown as {
            readonly name: string;
            readonly description: string;
            readonly parameters: { readonly properties: Record<string, object> };
            execute(args: SubagentArgs, api: ToolExecutionApi<never>, context: ToolContext): Promise<unknown>;
        };

        return {
            ...tool,
            description:
                builtIn.description +
                " Optional type: a preconfigured agent type (see the subagents section for the list); its brief is applied and wins over yours, and its model wins when it is allowed and can think at this session's level or lower. A model that is allowed but cannot think that low falls back to the session's model; one that is not allowed, or unknown, is refused by the built-in. Its thinking and tools apply where you set none.",
            parameters: Type.Object({
                ...builtIn.parameters.properties,
                thinking: Type.Optional(
                    Type.Union(
                        THINKING.map((level) => Type.Literal(level)),
                        // Keep whatever the built-in says about the level: it is what the model reads.
                        (builtIn.parameters.properties.thinking as { description?: string } | undefined)
                            ?.description === undefined
                            ? {}
                            : {
                                  description: (
                                      builtIn.parameters.properties.thinking as { description: string }
                                  ).description,
                              },
                    ),
                ),
                type: Type.Optional(
                    Type.String({
                        description: "spawn only: a preconfigured agent type by name, which brings its own model, thinking, tools, and brief.",
                    }),
                ),
            }),
            execute: async (args: SubagentArgs, api: ToolExecutionApi<never>, context: ToolContext) => {
                if (args.type === undefined) {
                    return builtIn.execute(args, api, context);
                }

                if (args.action !== "spawn") {
                    throw new Error("type applies to spawn only");
                }

                const type = args.type.trim().toLowerCase();
                const parent = await api.agent(context);
                const cwd = parent.cwd;
                const types = await loadTypes(cwd, host.agentDir);
                const definition = types.get(type);

                if (definition === undefined) {
                    throw new Error(
                        `No agent type named ${type}. Available: ${[...types.keys()].sort().join(", ") || "none: add a .md file to .pi/agents/ or ~/.pi/agent/agents/"}`,
                    );
                }

                if (definition.thinking !== undefined && !THINKING.includes(definition.thinking)) {
                    throw new Error(
                        `Agent type ${type}: thinking must be one of ${THINKING.join(", ")}, not ${definition.thinking}`,
                    );
                }

                const given = args.name?.trim();
                const next: SubagentArgs = { ...args };

                delete next.type;
                next.name = given === undefined || given === "" ? type : given;
                if (definition.model !== undefined) next.model = definition.model;
                if (definition.thinking !== undefined && args.thinking === undefined) next.thinking = definition.thinking;
                if (definition.tools !== undefined && args.tools === undefined) next.tools = definition.tools;
                if (definition.brief !== undefined && args.message?.trim() !== undefined) {
                    next.message = `${definition.brief}\n\n${args.message}`;
                }

                // The built-in refuses a foreign model that cannot think at or below the session's level.
                // A type that asks for one still works, on the session's model, rather than failing the
                // spawn: the built-in's started message names the model that actually runs. A model that
                // is unknown, disallowed, or the session's own is left to the built-in, so its refusal
                // (or its clamp) is what the caller sees.
                if (
                    definition.model !== undefined &&
                    fallsBackToSessionModel({
                        typeModel: definition.model,
                        parentModel: parent.model,
                        info: host.modelInfo(definition.model),
                        allowed:
                            typeof host.subagentModels === "function" ? host.subagentModels() : [],
                        asked:
                            definition.thinking !== undefined && args.thinking === undefined
                                ? definition.thinking
                                : args.thinking,
                        sessionLevel: parent.thinkingLevel ?? "off",
                    })
                ) {
                    delete next.model;
                }

                return builtIn.execute(next, api, context);
            },
        } as typeof tool;
    });

    const guide = wrapSection("subagents", (original) => ({
        ...original,
        render: async (input: PromptInput, context: ToolContext) => {
            const base = await original.render(input, context);
            const types = await loadTypes(input.agent.cwd, host.agentDir);

            if (types.size === 0) {
                return base;
            }

            const list = [...types.entries()]
                .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
                .map(
                    ([name, definition]) =>
                        `- ${name}` +
                        (definition.model === undefined ? "" : ` — model ${definition.model}`) +
                        (definition.thinking === undefined ? "" : `, thinking ${definition.thinking}`) +
                        (definition.tools === undefined ? "" : `, tools ${definition.tools.join(", ")}`) +
                        (definition.description === undefined ? "" : `: ${definition.description}`),
                )
                .join("\n");

            return (
                `${base ?? ""}\n` +
                "Preconfigured subagent types, for spawn with type=<name> (its brief wins over the caller's; its model wins when it is allowed and can think at this session's level or lower. A model that is allowed but cannot think that low runs on the session's model instead; one that is not allowed, or unknown, is refused by the built-in. Its thinking and tools apply where the caller set none; its name is the instance name unless one is given; the started message names the model actually used):\n" +
                `${list}\n` +
                "For non-trivial development work the intended order is architect (plan) → coder (implement) → reviewer (independent review), at most two review/fix cycles, then a report of what changed, what was run, and what is unresolved.\n" +
                "Each type is a .md file — model, thinking, tools, description frontmatter and a markdown brief that is prepended to the first message — in .pi/agents/ (unless the project is distrusted) or .agents/agents/ (only in a trusted project) of the session folder, or ~/.pi/agent/agents/ (first match wins; edit or add files to change the types)."
            );
        },
    }));

    return defineExtension({ name: "agent-types", wraps: [subagents, guide] });
}
