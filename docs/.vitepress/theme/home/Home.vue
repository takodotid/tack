<script setup lang="ts">
// The home page: the hull with a plan printing on it, the four phases as draft marks with the ledger filling in beside
// them, the reasons a run stops, the two modes, and the first command.

import { onBeforeUnmount, onMounted, ref } from "vue";
import { withBase } from "vitepress";
import { MODES, PHASES, PLAN, PLAN_COMMAND, REPOSITORY, STOPS } from "./home-content";

/** How many of the plan's lines are printed. All of them until the page is alive, so a page without scripts is whole. */
const printed = ref(PLAN.length);
/** The ledger's step now running, or PHASES.length once every step has succeeded. */
const running = ref(PHASES.length);
const copied = ref(false);
const ledger = ref<HTMLElement>();

let timers: number[] = [];
let observer: IntersectionObserver | undefined;

function later(run: () => void, ms: number) {
    timers.push(window.setTimeout(run, ms));
}

/** Print the plan a line at a time, the way the terminal does. */
function print() {
    printed.value = 0;
    PLAN.forEach((_, index) => later(() => (printed.value = index + 1), 500 + index * 150));
}

/** Fill the ledger in: each step is written as running, then as succeeded, before the next starts. */
function fill() {
    running.value = 0;
    PHASES.forEach((_, index) => later(() => (running.value = index + 1), 1100 * (index + 1)));
}

function state(index: number) {
    if (index < running.value) return "Succeeded";
    return index === running.value ? "Running" : "Pending";
}

async function copy() {
    try {
        await navigator.clipboard.writeText(PLAN_COMMAND);
        copied.value = true;
        later(() => (copied.value = false), 1800);
    } catch {
        // The command stays on the page to be selected by hand.
    }
}

onMounted(() => {
    if (matchMedia("(prefers-reduced-motion: reduce)").matches) return;

    print();

    observer = new IntersectionObserver(
        (entries) => {
            if (!entries.some((entry) => entry.isIntersecting)) return;
            observer?.disconnect();
            fill();
        },
        { threshold: 0.4 }
    );
    if (ledger.value) observer.observe(ledger.value);
});

onBeforeUnmount(() => {
    timers.forEach((timer) => clearTimeout(timer));
    observer?.disconnect();
});
</script>

<template>
    <article class="ship-home">
        <section class="sh-opening">
            <div class="sh-wrap sh-opening-grid">
                <div class="sh-opening-text">
                    <h1>Move a database primary between sites, on the operator's command.</h1>
                    <p class="sh-lede">
                        A planned switchover, or a failover after a site is lost, for CloudNativePG deployed by Flux. Every step is planned
                        first, recorded in a ledger, and resumable.
                    </p>
                    <p class="sh-actions">
                        <a class="sh-button sh-button-go" :href="withBase('/guide/getting-started')">
                            Get started
                            <svg viewBox="0 0 20 12" aria-hidden="true"><path d="M0 6h17M12 1l5 5-5 5" /></svg>
                        </a>
                        <a class="sh-button" :href="withBase('/guide/concepts')">How a run works</a>
                    </p>
                    <p class="sh-supports">
                        Tack supports CloudNativePG deployed by Flux kustomize-controller.
                        See <a :href="withBase('/limitations')">Limitations</a>.
                    </p>
                </div>

                <figure class="sh-sheet">
                    <figcaption>
                        <span class="sh-prompt">$</span> {{ PLAN_COMMAND }}
                    </figcaption>
                    <pre aria-label="An excerpt of what tack plan prints"><template v-for="(line, index) in PLAN" :key="index"><span
                        class="sh-line"
                        :class="[line.kind, { 'is-printed': index < printed }]"
                    >{{ line.text || " " }}
</span></template></pre>
                    <p class="sh-sheet-note">An excerpt, from <a :href="withBase('/guide/getting-started#what-a-plan-shows')">What a plan shows</a>.</p>
                </figure>
            </div>
        </section>

        <div class="sh-waterline" aria-hidden="true"></div>

        <section class="sh-plane">
            <div class="sh-wrap sh-chapter">
                <div class="sh-chapter-head">
                    <h2>A run moves the primary in four phases.</h2>
                    <p>
                        Each step is written to the ledger before it starts and after it ends. A stopped run resumes from its ledger.
                    </p>
                    <p><a :href="withBase('/guide/concepts')">Concepts</a> has every step.</p>
                </div>

                <ol ref="ledger" class="sh-marks">
                    <li v-for="(phase, index) in PHASES" :key="phase.name" :class="`is-${state(index).toLowerCase()}`">
                        <span class="sh-mark" aria-hidden="true">{{ index + 1 }}</span>
                        <div class="sh-mark-text">
                            <h3>{{ phase.name }}</h3>
                            <p>{{ phase.does }}</p>
                        </div>
                        <span class="sh-state">
                            <svg viewBox="0 0 14 14" aria-hidden="true">
                                <circle v-if="state(index) === 'Pending'" cx="7" cy="7" r="5" class="hollow" />
                                <path v-else-if="state(index) === 'Running'" d="M3 2l9 5-9 5z" />
                                <path v-else d="M1.5 7.5l3.5 3.5 7.5-8" class="tick" />
                            </svg>
                            {{ state(index) }}
                        </span>
                    </li>
                </ol>
            </div>
        </section>

        <section class="sh-black">
            <div class="sh-wrap sh-chapter">
                <div class="sh-chapter-head">
                    <h2>Anything unexpected stops the run, with a reason.</h2>
                    <p>
                        A stopped run exits with code 3, and one that waits for Git with code 4. Its objects stay held until the operator decides.
                        <code>tack resume</code> goes on from the ledger.
                    </p>
                </div>

                <ul class="sh-stops">
                    <li v-for="stop in STOPS" :key="stop.name">
                        <a :href="withBase(`/runbooks/stop-reasons#${stop.name.toLowerCase()}`)">{{ stop.name }}</a>
                        <span>{{ stop.when }}</span>
                    </li>
                </ul>
            </div>
        </section>

        <section class="sh-deck">
            <div class="sh-wrap sh-chapter">
                <div class="sh-chapter-head">
                    <h2>Two modes, and the operator chooses which.</h2>
                    <p>Tack never fails over by itself. What each mode can lose is in <a :href="withBase('/wal-rpo-rto')">Data loss and downtime</a>.</p>
                </div>

                <div class="sh-modes">
                    <div v-for="mode in MODES" :key="mode.flag">
                        <h3>{{ mode.name }}</h3>
                        <p class="sh-flag"><code>{{ mode.flag }}</code></p>
                        <dl>
                            <dt>When</dt>
                            <dd>{{ mode.when }}</dd>
                            <dt>What happens</dt>
                            <dd>{{ mode.happens }}</dd>
                        </dl>
                        <p><a :href="withBase(mode.link)">{{ mode.linkText }}</a></p>
                    </div>
                </div>
            </div>
        </section>

        <section class="sh-buff">
            <div class="sh-wrap sh-chapter">
                <div class="sh-chapter-head">
                    <h2>Start with a plan. It changes nothing.</h2>
                </div>
                <div class="sh-close-matter">
                    <button class="sh-command" type="button" @click="copy">
                        <span><span class="sh-prompt">$</span> {{ PLAN_COMMAND }}</span>
                        <span class="sh-copy" aria-live="polite">{{ copied ? "Copied" : "Copy" }}</span>
                    </button>
                    <p>
                        <a :href="withBase('/guide/install')">Install Tack</a>, follow <a :href="withBase('/guide/getting-started')">getting started</a>, read
                        <a :href="withBase('/limitations')">what Tack does not do</a>, or see the source on <a :href="REPOSITORY">GitHub</a>.
                    </p>
                </div>
            </div>
        </section>
    </article>
</template>
