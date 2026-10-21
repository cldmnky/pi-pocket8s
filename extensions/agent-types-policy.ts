/**
 * The parts of the agent-type extension that can be decided without Pi: which folders a project trust decision loads,
 * which thinking level a spawn asks for, and whether a type's model can run at the session's level. They live apart
 * from `agent-types.ts` so `node --test` can exercise them without the Pi runtime.
 */
import { join } from "node:path";

/**
 * Pi's thinking levels (`ModelThinkingLevel`), in its order. The built-in subagent schema offered only the first six
 * before 0.12.2, so a type could not ask for `max` — the only level some models have, such as Kimi K3 — though the
 * runtime took it. The wrapped schema rebuilds the union from this list and carries the level's description across.
 */
export const THINKING: readonly string[] = ["off", "minimal", "low", "medium", "high", "xhigh", "max"];

/** Pi's decision for a project: a saved answer, or undecided when there is none. */
export type ProjectTrust = "trusted" | "distrusted" | "undecided";

/** Where a level stands among the thinking levels, lowest first; -1 for a name this version does not know. */
export function rankThinking(level: string): number {
    return THINKING.indexOf(level);
}

/** A type's model reference and the thinking levels it offers, as `PocketHost.modelInfo` reports them. */
export type TypeModel = {
    readonly model: { readonly provider: string; readonly modelId: string };
    readonly thinkingLevels: readonly string[];
};

/**
 * The level a spawn asks for: the caller's, capped at the session's; a type's own only when the caller set none. A
 * level this version does not know is left for the built-in to judge rather than guessed at.
 */
export function wantedThinking(asked: string | undefined, sessionLevel: string): string {
    if (asked === undefined) {
        return sessionLevel;
    }

    const askedRank = rankThinking(asked);
    const sessionRank = rankThinking(sessionLevel);

    if (askedRank < 0 || sessionRank < 0) {
        return asked;
    }

    return askedRank > sessionRank ? sessionLevel : asked;
}

/** Whether `levels` contains one at or below `level`; the built-in refuses a model that cannot think that low. */
export function canThinkAtOrBelow(levels: readonly string[], level: string): boolean {
    const most = rankThinking(level);

    if (most < 0) {
        return true;
    }

    return levels.some((each) => {
        const rank = rankThinking(each);

        return rank >= 0 && rank <= most;
    });
}

/**
 * The agent folders a project trust decision opens: `.pi/agents` unless the project is distrusted (as `.pi/skills`
 * loads), and `.agents/agents` only in a trusted project (as `.agents/skills` loads). The shipped folder is separate.
 */
export function projectFolders(cwd: string | undefined, trust: ProjectTrust): string[] {
    if (cwd === undefined) {
        return [];
    }

    return [
        ...(trust === "distrusted" ? [] : [join(cwd, ".pi", "agents")]),
        ...(trust === "trusted" ? [join(cwd, ".agents", "agents")] : []),
    ];
}

/**
 * Whether a type's model must give way to the session's model: it is allowed, it is not the session's own model, and
 * it cannot think at the level the spawn asks for. A type model that is unknown or not allowed is left to the built-in
 * so its error, not a silent switch, reaches the caller.
 */
export function fallsBackToSessionModel(options: {
    readonly typeModel: string | undefined;
    readonly parentModel: { readonly provider: string; readonly modelId: string } | undefined;
    readonly info: TypeModel | undefined;
    readonly allowed: readonly string[];
    readonly asked: string | undefined;
    readonly sessionLevel: string;
}): boolean {
    const { typeModel, parentModel, info, allowed, asked, sessionLevel } = options;

    if (typeModel === undefined || parentModel === undefined || info === undefined) {
        return false;
    }

    const same =
        info.model.provider === parentModel.provider && info.model.modelId === parentModel.modelId;
    const key = `${info.model.provider}/${info.model.modelId}`;

    if (same || !allowed.includes(key)) {
        return false;
    }

    return !canThinkAtOrBelow(info.thinkingLevels, wantedThinking(asked, sessionLevel));
}
