// VitePress's default theme as a sister ship: the faces, self-hosted, the tokens, the rules, and a home page of our own.

import DefaultTheme from "vitepress/theme-without-fonts";
import type { Theme } from "vitepress";
import "@fontsource-variable/archivo/wdth.css";
import "@fontsource-variable/archivo/wdth-italic.css";
import "@fontsource/ibm-plex-mono/latin-400.css";
import "@fontsource/ibm-plex-mono/latin-600.css";
import Home from "./home/Home.vue";
import "./tokens.css";
import "./style.css";
import "./home/home.css";

export default {
    extends: DefaultTheme,
    enhanceApp({ app }) {
        app.component("Home", Home);
    },
} satisfies Theme;
