// The RFCs, read from rfcs/ when the site is built, so their list and their limitations on the site never drift from the
// files. Each is written as markdown into .vitepress/generated/, which pages include.

import { mkdirSync, readFileSync, readdirSync, writeFileSync } from "node:fs";
import { join, posix } from "node:path";

/** The repository on GitHub, where a link that leaves the site goes. */
export const REPOSITORY = "https://github.com/takodotid/tack";

type Rfc = { file: string; number: string; title: string; version: string; status: string; abstract: string; limitations: string };

/** Each `# Name` section of an RFC, by name. */
function sections(text: string): Map<string, string> {
    const out = new Map<string, string>();
    let name = "";
    for (const line of text.split("\n")) {
        const heading = /^# (.+)$/.exec(line);
        if (heading) {
            name = heading[1].trim();
            out.set(name, "");
        } else if (name) {
            out.set(name, out.get(name) + line + "\n");
        }
    }
    return out;
}

/**
 * A link of an RFC, as the site serves it: a page of docs/ by its address on the site, any other file of the repository
 * on GitHub. An `[RFC#NNNN]` that is not a link becomes one.
 */
function siteLinks(markdown: string): string {
    return markdown
        .replace(/\]\((?!https?:|#|mailto:)([^)\s]+)\)/g, (_, target: string) => {
            const [path, anchor] = target.split("#");
            const resolved = posix.normalize(posix.join("rfcs", path));
            const hash = anchor ? `#${anchor}` : "";
            if (resolved.startsWith("docs/") && resolved.endsWith(".md")) {
                return `](/${resolved.slice("docs/".length, -".md".length)}${hash})`;
            }
            return `](${REPOSITORY}/blob/main/${resolved}${hash})`;
        })
        .replace(/\[RFC#(\d{4})\](?!\()/g, (_, n: string) => `[RFC#${n}](${REPOSITORY}/blob/main/rfcs/RFC${n}.md)`);
}

/** Every RFC of the directory, in order. */
export function readRfcs(dir: string): Rfc[] {
    return readdirSync(dir)
        .filter((f) => /^RFC\d{4}\.md$/.test(f))
        .sort()
        .map((file) => {
            const text = readFileSync(join(dir, file), "utf8").replace(/\r\n/g, "\n");
            const title = /\[RFC#(\d{4})\]\s*([^<]+)</.exec(text);
            const s = sections(text);
            const status = s.get("Status") ?? "";
            return {
                file,
                number: title?.[1] ?? file.slice(3, 7),
                title: (title?.[2] ?? file).trim(),
                version: /- Version:\s*(\S+)/.exec(status)?.[1] ?? "",
                status: /- Status:\s*(\S+)/.exec(status)?.[1] ?? "",
                abstract: (s.get("Abstract") ?? "").trim().split(/\n\s*\n/)[0] ?? "",
                limitations: (s.get("Limitations") ?? "").trim(),
            };
        });
}

/** Writes the list of RFCs and their limitations as markdown, for the pages that include them. */
export function generate(rfcDir: string, outDir: string): void {
    const rfcs = readRfcs(rfcDir);
    mkdirSync(outDir, { recursive: true });

    const list = [
        "| RFC | Subject | Status | Version |",
        "|---|---|---|---|",
        ...rfcs.map((r) => `| [RFC#${r.number}](${REPOSITORY}/blob/main/rfcs/${r.file}) | ${r.title} | ${r.status} | ${r.version} |`),
        "",
        ...rfcs.flatMap((r) => [`### RFC#${r.number}: ${r.title}`, "", siteLinks(r.abstract), ""]),
    ].join("\n");
    writeFileSync(join(outDir, "rfcs.md"), list);

    const limitations = rfcs
        .flatMap((r) => [`## RFC#${r.number}: ${r.title}`, "", siteLinks(r.limitations), "", `From [RFC#${r.number}](${REPOSITORY}/blob/main/rfcs/${r.file}#limitations).`, ""])
        .join("\n");
    writeFileSync(join(outDir, "limitations.md"), limitations);
}
