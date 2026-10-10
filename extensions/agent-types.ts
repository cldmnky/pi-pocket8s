/**
 * Preconfigured subagent types: markdown files with a model, thinking level, tool list, and brief, applied when the
 * built-in subagent tool spawns with `type`. Files come from `.pi/agents/` or `.agents/agents/` in the session folder,
 * or `~/.pi/agent/agents/` everywhere (the first match wins), in the same layout as `pi`'s subagents use, so a file
 * works in both.
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
import type { PocketHost } from "/opt/pi-pocket/src/server/host.ts";

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

/**
 * Pi's own thinking levels (`ModelThinkingLevel`), in its order. The built-in subagent tool's schema
 * offers only the first six, so a type could not ask for `max` — the one level some models have, such
 * as Kimi K3 — even though the runtime takes it. The wrapped schema below widens the union back.
 */
const THINKING = ["off", "minimal", "low", "medium", "high", "xhigh", "max"];

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

/** Every agent type in scope for a session folder, by name; a project file shadows a global one with the same name. */
async function loadTypes(cwd: string | undefined, agentDir: string): Promise<Map<string, AgentType>> {
    const folders = cwd === undefined ? [] : [join(cwd, ".pi", "agents"), join(cwd, ".agents", "agents")];
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
                " Optional type: a preconfigured agent type (see the subagents section for the list); its model and brief are applied and win over yours, its thinking and tools where you set none.",
            parameters: Type.Object({
                ...builtIn.parameters.properties,
                thinking: Type.Optional(Type.Union(THINKING.map((level) => Type.Literal(level)))),
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
                const cwd = (await api.agent(context)).cwd;
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
                next.name = given === undefined || given === "" ? type : args.name;
                if (definition.model !== undefined) next.model = definition.model;
                if (definition.thinking !== undefined && args.thinking === undefined) next.thinking = definition.thinking;
                if (definition.tools !== undefined && args.tools === undefined) next.tools = definition.tools;
                if (definition.brief !== undefined && args.message?.trim() !== undefined) {
                    next.message = `${definition.brief}\n\n${args.message}`;
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
                "Preconfigured subagent types, for spawn with type=<name> (the type's model and brief win over the caller's; its thinking and tools apply where the caller set none; its name is the instance name unless one is given):\n" +
                `${list}\n` +
                "For non-trivial development work the intended order is architect (plan) → coder (implement) → reviewer (independent review), at most two review/fix cycles, then a report of what changed, what was run, and what is unresolved.\n" +
                "Each type is a .md file — model, thinking, tools, description frontmatter and a markdown brief that is prepended to the first message — in .pi/agents/ or .agents/agents/ of the session folder, or ~/.pi/agent/agents/ (first match wins; edit or add files to change the types)."
            );
        },
    }));

    return defineExtension({ name: "agent-types", wraps: [subagents, guide] });
}
