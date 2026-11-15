// What the home page says. Each line is taken from a page of the docs, which stays the authority: the plan from
// guide/getting-started.md, the phases and modes from guide/concepts.md, the stops from runbooks/stop-reasons.md.

export const REPOSITORY = "https://github.com/takodotid/tack";

export const PLAN_COMMAND = "tack plan -f workload.yaml --to b";

type Line = { text: string; kind?: "head" | "step" | "soft" | "done" };

export const PLAN: Line[] = [
    { text: "Plan for workload pg-main: Planned, site a to site b", kind: "head" },
    { text: "Ledger: SiteFailover tack-system/pg-main-20261027-101500 at site b", kind: "soft" },
    { text: "" },
    { text: "Failover", kind: "head" },
    { text: "   9. demote-a  site a", kind: "step" },
    { text: "      demote Cluster pg-a: declare pg-b primary" },
    { text: "  10. wait-demoted-a  site a", kind: "step" },
    { text: "      wait until Cluster pg-a has published its demotion token" },
    { text: "  11. capture-token  site a", kind: "step" },
    { text: "      record the demotion token of Cluster pg-a in the ledger" },
    { text: "  12. promote-b  site b", kind: "step" },
    { text: "      promote Cluster pg-b with the demotion token, in one patch" },
    { text: "" },
    { text: "PromoteToGitOps", kind: "head" },
    { text: "  18. git-commit  Git", kind: "step" },
    { text: "      commit message: tack: pg-main primary a to b (planned)" },
    { text: "" },
    { text: "Nothing was changed.", kind: "done" },
];

export const PHASES = [
    {
        name: "Hold",
        does: "Stops Flux from applying the live objects the run changes, at each site, and no others.",
    },
    {
        name: "Failover",
        does: "Stops the old Pooler, demotes the old primary, captures its demotion token, promotes the new primary with it, starts the new Pooler.",
    },
    {
        name: "Promote to GitOps",
        does: "Writes one commit that makes Git match live, then waits until Flux at each site has applied it.",
    },
    {
        name: "Release",
        does: "Renders the commit the way Flux does and compares it with live. Lets Flux apply the objects again only if the two match.",
    },
];

export const STOPS = [
    { name: "GitUnreachable", when: "Git cannot be fetched or pushed. The run waits." },
    { name: "GitChanged", when: "The branch moved away from the run's base." },
    { name: "GitDiffersFromLive", when: "Before release, Git as Flux renders it differs from live." },
    { name: "TokenMissing", when: "The demotion token is gone, or is no longer the one captured." },
    { name: "FencedSiteIsPrimary", when: "Going on would leave two primaries." },
    { name: "HeldByAnotherRun", when: "An object is held by another run, or by hand." },
    { name: "FluxChanged", when: "After release, Flux applied something to an object." },
    { name: "SiteMismatch", when: "A context reaches a different cluster than the run recorded." },
    { name: "StepFailed", when: "Any other error. Fix the cause, then resume." },
];

export const MODES = [
    {
        name: "Planned",
        flag: "--mode planned",
        when: "Both sites answer. This is the default.",
        happens: "The old primary demotes and hands its token to the new one. No write is lost.",
        link: "/guide/concepts#two-modes",
        linkText: "The two modes",
    },
    {
        name: "Site loss",
        flag: "--mode site-loss",
        when: "The old site is lost, and you have confirmed the loss.",
        happens: "The new site promotes without a token. The old primary must be rebuilt before it can follow the new one again.",
        link: "/runbooks/site-loss",
        linkText: "Fail over after a site loss",
    },
];
