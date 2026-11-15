import { readFileSync } from "node:fs";
import { posix } from "node:path";
import { fileURLToPath } from "node:url";
import { slug } from "github-slugger";
import { defineConfig, type DefaultTheme } from "vitepress";
import { REPOSITORY, generate } from "./rfcs.ts";
import { Seo } from "./seo.ts";

const HOSTNAME = "https://tack.tako.id";
const DESCRIPTION =
    "Tack moves the primary of a CloudNativePG database from one Kubernetes site to another, keeps Flux and Git in step, and records every step in a ledger.";

// The RFCs' list and limitations, written for the pages that include them.
generate(fileURLToPath(new URL("../../rfcs", import.meta.url)), fileURLToPath(new URL("./generated", import.meta.url)));

const sidebar: DefaultTheme.SidebarItem[] = [
    {
        text: "Guide",
        items: [
            { text: "Introduction", link: "/guide/introduction" },
            { text: "Install", link: "/guide/install" },
            { text: "Getting started", link: "/guide/getting-started" },
            { text: "Concepts", link: "/guide/concepts" },
            { text: "Upgrade", link: "/guide/upgrade" },
        ],
    },
    {
        text: "Operations",
        items: [
            { text: "Running Tack", link: "/operations" },
            { text: "Data loss and downtime", link: "/wal-rpo-rto" },
            { text: "Fail over after a site loss", link: "/runbooks/site-loss" },
            { text: "Recover the old primary's writes", link: "/runbooks/recover-old-primary-writes" },
            { text: "Rebuild an old primary", link: "/runbooks/rebuild-old-primary" },
            { text: "Stopped and waiting runs", link: "/runbooks/stop-reasons" },
            { text: "No primary after demotion", link: "/runbooks/no-primary-after-demotion" },
            { text: "Abandon a run", link: "/runbooks/abandon-run" },
            { text: "Measure failover time", link: "/runbooks/measure-failover-time" },
        ],
    },
    {
        text: "Reference",
        items: [
            { text: "The tack command", link: "/reference/cli" },
            { text: "The Workload file", link: "/workload" },
            { text: "The ledger", link: "/reference/ledger" },
            { text: "Metrics", link: "/reference/metrics" },
            { text: "RFCs", link: "/reference/rfcs" },
        ],
    },
    {
        text: "Limitations",
        items: [{ text: "Limitations", link: "/limitations" }],
    },
    {
        text: "Project",
        items: [
            { text: "Contributing", link: "/contributing" },
            { text: "Releasing Tack", link: "/runbooks/release" },
        ],
    },
];

/**
 * A link from a page of docs/ to a file outside it, such as an RFC, goes to that file on GitHub: the pages keep the
 * relative links that work in the repository.
 */
function repositoryLinks(md: any) {
    md.core.ruler.after("inline", "tack-repository-links", (state: any) => {
        const page: string | undefined = state.env?.relativePath;
        if (!page) return;
        for (const token of state.tokens) {
            for (const child of token.children ?? []) {
                if (child.type !== "link_open") continue;
                const href: string = child.attrGet("href") ?? "";
                if (!href || /^([a-z][a-z0-9+.-]*:|#|\/)/i.test(href)) continue;
                const [path, anchor] = href.split("#");
                const resolved = posix.normalize(posix.join("docs", posix.dirname(page), path));
                if (resolved.startsWith("docs/")) continue;
                const kind = path.endsWith("/") ? "tree" : "blob";
                child.attrSet("href", `${REPOSITORY}/${kind}/main/${resolved.replace(/\/$/, "")}${anchor ? `#${anchor}` : ""}`);
            }
        }
    });
}

/** The site's colours, from tokens.css, the one file that holds them. */
function tokens(): Record<string, string> {
    const css = readFileSync(new URL("./theme/tokens.css", import.meta.url), "utf8");
    const body = /:root\s*\{([^}]*)\}/.exec(css)?.[1] ?? "";
    return Object.fromEntries([...body.matchAll(/--ship-([a-z-]+):\s*(#[0-9a-f]{6})/g)].map((m) => [m[1], m[2]]));
}
const ship = tokens();

/** Code on the black hull, in both themes: light text, comments in soft, keywords in the funnel's buff. */
const codeTheme = {
    name: "ship",
    type: "dark",
    colors: { "editor.background": ship["code-bg"], "editor.foreground": ship["code-text"] },
    tokenColors: [
        { settings: { foreground: ship["code-text"] } },
        { scope: ["comment", "punctuation.definition.comment"], settings: { foreground: ship["code-soft"], fontStyle: "italic" } },
        { scope: ["keyword", "storage", "entity.name.tag", "support.function.builtin"], settings: { foreground: ship["code-accent"] } },
    ],
};

export default defineConfig({
    title: "Tack",
    description: DESCRIPTION,
    lang: "en",
    cleanUrls: true,
    lastUpdated: true,

    sitemap: { hostname: HOSTNAME },

    head: [
        ["meta", { name: "robots", content: "index, follow, max-snippet:-1" }],
        ["meta", { name: "theme-color", content: ship.hull }],
    ],

    // A page without a description of its own is described by its first paragraph.
    transformPageData(pageData, { siteConfig }) {
        if (pageData.frontmatter.description || pageData.isNotFound) return;
        const description = Seo.describe(`${siteConfig.srcDir}/${pageData.relativePath}`);
        if (description) pageData.description = description;
    },

    transformHead: ({ pageData }) =>
        Seo.head(pageData, { hostname: HOSTNAME, name: "Tack", description: DESCRIPTION, repository: REPOSITORY, sidebar }),

    markdown: {
        // Anchors as GitHub makes them, so a link to a heading works in the repository and on the site alike.
        anchor: { slugify: slug },
        headers: { slugify: slug },
        theme: codeTheme as any,
        config: repositoryLinks,
    },

    themeConfig: {
        nav: [
            { text: "Guide", link: "/guide/introduction", activeMatch: "^/guide/" },
            { text: "Operations", link: "/operations", activeMatch: "^/(operations|wal-rpo-rto|runbooks/)" },
            { text: "Reference", link: "/reference/cli", activeMatch: "^/(reference/|workload)" },
            { text: "Limitations", link: "/limitations" },
            { text: "Releases", link: `${REPOSITORY}/releases` },
        ],

        sidebar,

        socialLinks: [{ icon: "github", link: REPOSITORY }],

        editLink: { pattern: `${REPOSITORY}/edit/main/docs/:path` },

        outline: { level: [2, 3], label: "On this page" },

        search: {
            provider: "local",
            options: { translations: { button: { buttonText: "Search docs" } } },
        },

        footer: {
            message: "Released under the Apache License 2.0.",
        },
    },
});
