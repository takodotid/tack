// VitePress's default theme in the chart table: the fonts, self-hosted, then the tokens and the rules that use them.

import DefaultTheme from "vitepress/theme-without-fonts";
import type { Theme } from "vitepress";
import "@fontsource/spectral/latin-400.css";
import "@fontsource/spectral/latin-400-italic.css";
import "@fontsource/spectral/latin-600.css";
import "@fontsource/spectral/latin-700.css";
import "@fontsource/ibm-plex-sans-condensed/latin-600.css";
import "@fontsource/ibm-plex-mono/latin-400.css";
import "@fontsource/ibm-plex-mono/latin-600.css";
import "./tokens.css";
import "./style.css";

export default {
    extends: DefaultTheme,
} satisfies Theme;
