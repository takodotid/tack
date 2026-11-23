---
name: Tack
description: A ship's livery for the documentation sites of Tack and Strait. A hull colour, superstructure white, a black band, one buff for actions, set in Archivo across its width axis.
colors:
    hull: "#a2321d"
    hull-deep: "#7d2313"
    on-hull: "#fff8ee"
    on-hull-soft: "#f6d3c7"
    plane: "#fbfaf5"
    deck: "#f0ece3"
    line: "#d9d3c6"
    ink: "#1a1512"
    soft-ink: "#5f564e"
    buff: "#edb23c"
    buff-deep: "#dc9d1f"
    link: "#a2321d"
    link-hover: "#7d2313"
    code-bg: "#1a1512"
    code-text: "#f1eadc"
    code-soft: "#a89e90"
    code-accent: "#edb23c"
    code-line: "#3a322c"
    band: "#1a1512"
    band-soft: "#c9c0b1"
    band-line: "#3a322c"
    warning: "#a85400"
    tip: "#1b6e45"
    danger: "#b3261e"
    night-plane: "#15110f"
    night-deck: "#1e1917"
    night-line: "#3a322c"
    night-ink: "#f4eee2"
    night-soft-ink: "#b9aea1"
    night-link: "#f39c84"
    night-link-hover: "#f8bfae"
    night-code-bg: "#0c0a09"
    night-band: "#5e190c"
    night-band-soft: "#f6d3c7"
    night-band-line: "#8a3524"
    night-warning: "#f0a24f"
    night-tip: "#5cc98d"
    night-danger: "#ff8a78"
typography:
    headline:
        fontFamily: "Archivo Variable, Segoe UI, system-ui, sans-serif"
        fontWeight: 750
        fontStretch: "106%"
        letterSpacing: "-0.015em"
    title:
        fontFamily: "Archivo Variable, Segoe UI, system-ui, sans-serif"
        fontSize: "42px"
        fontWeight: 800
        lineHeight: 1.1
    body:
        fontFamily: "Archivo Variable, Segoe UI, system-ui, sans-serif"
        fontSize: "16.5px"
        fontWeight: 400
        lineHeight: 1.72
    label:
        fontFamily: "Archivo Variable, Segoe UI, system-ui, sans-serif"
        fontSize: "13.5px"
        fontWeight: 700
        fontStretch: "78%"
        letterSpacing: "0.07em"
        textTransform: "uppercase"
    wordmark:
        fontFamily: "Archivo Variable, Segoe UI, system-ui, sans-serif"
        fontSize: "21px"
        fontWeight: 800
        fontStretch: "125%"
        letterSpacing: "0.06em"
        textTransform: "uppercase"
    numeral:
        fontFamily: "Archivo Variable, Segoe UI, system-ui, sans-serif"
        fontSize: "76px"
        fontWeight: 800
        fontStretch: "62%"
    mono:
        fontFamily: "IBM Plex Mono, ui-monospace, Menlo, Consolas, monospace"
        fontWeight: 400
rounded:
    none: "0px"
    control: "3px"
borders:
    hairline: "1px"
    rule: "3px"
    cap: "4px"
components:
    masthead:
        backgroundColor: "{colors.hull}"
        textColor: "{colors.on-hull}"
        borderColor: "{colors.ink}"
        typography: "{typography.label}"
    button-primary:
        backgroundColor: "{colors.buff}"
        textColor: "{colors.ink}"
        rounded: "{rounded.control}"
    button-primary-hover:
        backgroundColor: "{colors.buff-deep}"
        textColor: "{colors.ink}"
    button-default:
        backgroundColor: "{colors.plane}"
        textColor: "{colors.ink}"
        borderColor: "{colors.ink}"
        rounded: "{rounded.control}"
    table-head:
        backgroundColor: "{colors.band}"
        textColor: "{colors.on-hull}"
        typography: "{typography.label}"
    table-row:
        borderColor: "{colors.line}"
        rounded: "{rounded.none}"
    code-block:
        backgroundColor: "{colors.code-bg}"
        textColor: "{colors.code-text}"
        typography: "{typography.mono}"
        rounded: "{rounded.control}"
    code-inline:
        backgroundColor: "{colors.deck}"
        textColor: "{colors.ink}"
        borderColor: "{colors.line}"
        rounded: "{rounded.control}"
    callout:
        backgroundColor: "{colors.deck}"
        textColor: "{colors.ink}"
        borderWidth: "{borders.cap}"
        rounded: "{rounded.none}"
    callout-tip:
        borderColor: "{colors.tip}"
    callout-warning:
        borderColor: "{colors.warning}"
    callout-danger:
        borderColor: "{colors.danger}"
    badge:
        backgroundColor: "{colors.deck}"
        textColor: "{colors.ink}"
        borderColor: "{colors.line}"
        typography: "{typography.label}"
        rounded: "{rounded.control}"
    hero:
        backgroundColor: "{colors.hull}"
        textColor: "{colors.on-hull}"
    footer:
        backgroundColor: "{colors.code-bg}"
        textColor: "{colors.code-soft}"
---

# Design System: Tack

## Overview

**Creative North Star: "Sister Ships"**

The docs wear a ship's livery. The hull's colour fills the masthead and the home page's opening. Below the waterline, the page is superstructure white, with the deck as the second plane, a black band, and the funnel's buff for the one action. Tack and Strait are sister ships: they share every rule and name here, and differ only in the hull and its tints. Tack's hull is red.

Colour comes in whole planes, not in accents. Type is one family, Archivo, across its width axis: wide for the name, a little wide for headings, normal for reading, narrow for markings, as stencilled on plate. Code sits on the black hull in both themes.

At night the planes turn dark and the ink light. The hull, buff and the ground of code stay as they are.

## Colors

Every colour is a token in `docs/.vitepress/theme/tokens.css`, named by its role. `style.css` maps VitePress's own variables to them.

| Token | Day | Night | Used for |
|---|---|---|---|
| `hull` | `#a2321d` | same | The masthead, the home page's opening, the phase numerals by day. |
| `hull-deep` | `#7d2313` | same | The search box and the switch on the masthead. |
| `on-hull` | `#fff8ee` | same | Text on the hull and on the band. |
| `on-hull-soft` | `#f6d3c7` | same | Secondary text and lines on the hull. |
| `plane` | `#fbfaf5` | `#15110f` | The page. |
| `deck` | `#f0ece3` | `#1e1917` | The second plane: callouts, a word of code, badges, the current page in the sidebar. |
| `line` | `#d9d3c6` | `#3a322c` | Hairlines: table rows, dividers. Never the only mark of a state. |
| `ink` | `#1a1512` | `#f4eee2` | Text, rules, the band under the masthead. |
| `soft-ink` | `#5f564e` | `#b9aea1` | Secondary text. |
| `buff` | `#edb23c` | same | The primary button, the active nav item's bar, the title mark, the closing plane of the home page, keywords in code. |
| `buff-deep` | `#dc9d1f` | same | The primary button on hover. |
| `link` | `#a2321d` | `#f39c84` | Links, and the current page in the sidebar. |
| `link-hover` | `#7d2313` | `#f8bfae` | A link on hover. |
| `code-bg` | `#1a1512` | `#0c0a09` | The ground of a code block, and the footer. |
| `code-text`, `code-soft`, `code-accent` | `#f1eadc`, `#a89e90`, `#edb23c` | same | Code, its comments, its keywords. |
| `band` | `#1a1512` | `#5e190c` | The dark field: a table's head, the stops on the home page. Black by day, the hull's deep red at night. |
| `band-soft`, `band-line` | `#c9c0b1`, `#3a322c` | `#f6d3c7`, `#8a3524` | Secondary text and lines on the band. |
| `tip` | `#1b6e45` | `#5cc98d` | Green: a tip, and what is done. |
| `warning` | `#a85400` | `#f0a24f` | Orange: a warning, and what is running. |
| `danger` | `#b3261e` | `#ff8a78` | Red: split brain, data loss, a destructive step. |

Text on buff is always `#1a1512`, in both themes.

### Named Rules

**The One Action Rule.** Buff is the only action colour. There is no second accent.

**The Plane Rule.** A colour fills a whole region or marks one thing. There is no gradient, no glow and no blur.

**The Red Means Danger Rule.** The state red is for danger only. Warnings are orange. The hull's red is livery, never a state. Every state also carries its word or an icon, so it reads without colour.

**The Black Hull Rule.** A block of code sits on the black hull in both themes.

## Typography

**Family:** Archivo Variable, with its width axis, for everything but code. **Mono:** IBM Plex Mono, 400 and 600. Both are open-licensed and self-hosted with the site, from the @fontsource packages: no font is fetched from another host.

| Role | Weight | Width | Size | Used for |
|---|---|---|---|---|
| Wordmark | 800 | 125% | 21px, capitals, tracked 0.06em | "Tack" on the masthead. There is no logo yet. |
| Page title | 800 | 106% | 42px, 32px on a phone | The first heading of a page, with the title mark under it. |
| Heading | 750 | 106% | 27px and 20px, 24px for the first on a phone | Section headings, in sentence case, tracked -0.015em. |
| Home headline | 800 | 112% | 36px to 60px, 28px to 56px in one column | The home page's opening. It keeps to four lines beside the plan, and to five on a phone. |
| Body | 400 | 100% | 16.5px, line height 1.72 | Reading text, at most 34em wide. |
| Link | 550 | 100% | as its text | Underlined, 1.5px, thicker on hover. |
| Marking | 700 | 78% | 13.5px, capitals, tracked 0.07em | Nav links, sidebar groups, table heads, callout titles, badges, the outline's title, a cell's name on a phone. |
| Numeral | 800 | 62% | 76px | The phase numbers on the home page, and "404". |
| Mono | 400, 600 | | 0.86em in text | Code, commands, field names. |

### Named Rules

**The Markings Rule.** Capitals are only for short labels, set narrow and tracked 0.07em. Headings and reading text stay in sentence case. A marking labels a control, a state or a column. It never sits over a heading as decoration.

**The Width Rule.** Hierarchy comes from width and weight within the one family: wide for the name, narrow for markings and numerals. No second family is added for display.

## Layout

VitePress's documentation layout: masthead, sidebar, page and outline.

| Part | Rule |
|---|---|
| Masthead | The hull's colour on every page, 68px high, ending on a 4px ink band. On the home page it runs into the opening, and the band comes at the waterline. |
| Page | Up to 784px wide beside the outline. Reading text keeps to 34em, so a table or a block of code is wider than the text. |
| Page title | Ends on the title mark: a buff bar 64px by 10px under a 4px ink cap. |
| Section | A second-level heading starts on a 1px line, 56px below what came before. |
| Home page | Planes in order: hull, a waterline of white then ink, white, black band, deck, buff. Each chapter has a heavy heading at the left and its matter at the right, and is one column below 901px. |
| Phone menu | Below 768px the nav is a menu. Its links are markings. |

## Elevation & Depth

Flat. Depth comes from the planes and from lines.

| Shadow | Where |
|---|---|
| `0 24px 48px -12px` in ink at 55% | The plan sheet on the home page's opening, and nothing else in the theme's own parts. |
| `0 10px 28px` in ink at 18% | What VitePress floats: menus and the search box. |

There is no other shadow.

## Motion

| Motion | Where |
|---|---|
| The plan prints a line at a time, and the four phases fill in as the ledger does | The home page, once. |
| Colour and arrow on hover, 0.2s to 0.3s | Buttons and states on the home page. |
| VitePress's own | Menus, the sidebar, the theme switch. |

With `prefers-reduced-motion`, the plan and the phases are whole from the start and the transitions are off. Nothing loops and nothing pulses. The page is whole without scripts.

## Shapes

| Shape | Radius |
|---|---|
| Buttons, code blocks, a word of code, badges, the search box, the current page in the sidebar, focus rings | 3px, the token `ship-radius` |
| Tables, callouts, planes, the title mark | 0 |

A thick border never sits on a rounded corner: a callout has a 4px cap, so it is square.

## Components

### Buttons

The primary button is buff with ink text, and darkens on hover. The others are the plane with an ink frame and ink text, and take the deck on hover. On the hull, the second button has a frame in `on-hull` and inverts on hover.

### Tables

Set as a timetable: a head on the band with markings, hairline rows, no frame, figures that line up. The first column is heavier.

| Case | Rule |
|---|---|
| A table with an empty head | Shows no head. Its first row starts on a 3px ink rule. |
| Two or three columns | The first column keeps at least 13em or 8em. |
| Five columns or more | Set closer: 14px, with less padding. |
| A word of code of 25 characters or more | May break, after `_ . / , = :` first. A shorter word never breaks. `config.ts` marks it. |
| Below 720px | No columns. Each row is a record: its cells one under the other, each after its column's name as a marking. `config.ts` gives each cell that name, and each part of the table its role for a screen reader. |

A table never scrolls sideways.

### Code

A block is light text on the black hull, with comments soft and keywords in buff. A word of code in the text is ink on the deck, in a hairline frame.

### Callouts

A deck plane under a 4px cap, square. The cap and the title take the state's colour: green for a tip, orange for a warning, red for danger, ink for information. The title is the state's word, as a marking. The text is ink.

### Badges

Ink on the deck in a hairline frame, as a marking. The word carries the state.

### Navigation

The wordmark, lettered wide. Nav links and sidebar groups are markings. The active nav item has a 6px buff bar under it. The current page in the sidebar is on the deck, in the link colour, heavier. A long entry of the outline wraps.

### Not found

"404" as a numeral in the link colour, "Page not found" as a heading, one line of help, and a framed button to the home page.

## Contrast

Every pair meets WCAG AA, computed with the WCAG 2 formula from the tokens: 4.5:1 for text, 3:1 for a mark that is not text. The one exception is a nav link on hover: `buff` on `hull`, 3.67:1. At rest that text reads at 6.62:1.

| Use | Pair | Day | Night |
|---|---|---|---|
| Text | `ink` on `plane` | 17.32 | 16.24 |
| Text on the deck | `ink` on `deck` | 15.36 | 15.06 |
| Secondary text | `soft-ink` on `plane` | 6.86 | 8.61 |
| Secondary text on the deck | `soft-ink` on `deck` | 6.08 | 7.98 |
| Links | `link` on `plane` | 6.68 | 8.85 |
| Links on the deck, the current page | `link` on `deck` | 5.92 | 8.20 |
| Link hover | `link-hover` on `plane` | 9.47 | 11.67 |
| Text on the hull | `on-hull` on `hull` | 6.62 | 6.62 |
| Secondary text on the hull | `on-hull-soft` on `hull` | 5.00 | 5.00 |
| Text in the search box | `on-hull` on `hull-deep` | 9.39 | 9.39 |
| Text on buff | `#1a1512` on `buff` | 9.51 | 9.51 |
| Text on a hovered primary button | `#1a1512` on `buff-deep` | 7.66 | 7.66 |
| Code | `code-text` on `code-bg` | 15.12 | 16.50 |
| Comments in code, the footer | `code-soft` on `code-bg` | 6.86 | 7.49 |
| Keywords in code | `code-accent` on `code-bg` | 9.51 | 10.38 |
| A table's head | `on-hull` on `band` | 17.17 | 12.29 |
| Secondary text on the band | `band-soft` on `band` | 10.05 | 9.30 |
| Stop reasons on the band | `buff` on `band` | 9.51 | 6.81 |
| A tip's title | `tip` on `deck` | 5.30 | 8.45 |
| A warning's title | `warning` on `deck` | 4.53 | 8.28 |
| A danger's title | `danger` on `deck` | 5.54 | 7.59 |
| A state on the home page | `tip` or `warning` on `plane` | 5.98, 5.11 | 9.11, 8.92 |
| The active nav item's bar, 3:1 | `buff` on `hull` | 3.67 | 3.67 |

## Do's and Don'ts

### Do:

- **Do** take every colour, face and radius from `docs/.vitepress/theme/tokens.css`, by its role.
- **Do** check a new page at 390px and at 1280px.
- **Do** write the answer first, in short sentences of everyday English.

### Don't:

- **Don't** set text in `line`, or in buff on the hull at rest.
- **Don't** use em-dashes in any copy.
