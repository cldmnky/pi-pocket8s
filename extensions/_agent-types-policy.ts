/**
 * The leading `_` keeps Pi Pocket's extension loader from loading this helper as an extension module.
 *
 * The parts of the agent-type extension that can be decided without Pi: which folders a project trust decision loads,
 * whether a spawn's own thinking level passes the session's, and whether a type's model may give way to the session's.
 * They live apart from `agent-types.ts` so `node --test` can exercise them without the Pi runtime.
 */
import { join } from "node:path";

/**
 * Pi's thinking levels (`ModelThinkingLevel`), in its order. The wrapped schema rebuilds the built-in's union from this
 * list, so a type file is judged against the same names here, and the level's description is carried across for the
 * model to read. A level Pi adds shows up here as an unknown one, which is left to the built-in to judge.
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
 * The level a spawn is compared against: the caller's, capped at the session's. This is the local fallback policy of
 * `fallsBackToSessionModel`, not an upstream rule: 0.25 dropped the old cap on a parent's thinking level and lowers a
 * requested one to the nearest supported at or below it, else the model's lowest. Keeping the comparison no higher
 * than the session's is what makes a fallback a safe substitution: the session's model already runs at its own level.
 * A level this version does not know is left for the built-in to judge rather than guessed at.
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

/** Whether `levels` contains one at or below `level`: a model without one cannot run at that height at all. */
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
 * Whether a type's model must give way to the session's model: both are allowed, the type's is not the session's own,
 * and it cannot think at the level the spawn is compared against. A type model that is unknown or not allowed is left
 * to the built-in so its error, not a silent switch, reaches the caller.
 *
 * The session's model has to be allowed too. With no allow-list the built-in accepts only the session's own model, and
 * with a nonempty one only the listed models: in both cases dropping the type's model for a session model off the list
 * would turn the built-in's clear refusal into a second, worse failure.
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
    const parentKey = `${parentModel.provider}/${parentModel.modelId}`;
    // An empty list puts nothing off limits here: the check below already refuses a type model that is not listed.
    const parentAllowed = allowed.length === 0 || allowed.includes(parentKey);

    if (same || !allowed.includes(key) || !parentAllowed) {
        return false;
    }

    return !canThinkAtOrBelow(info.thinkingLevels, wantedThinking(asked, sessionLevel));
}
