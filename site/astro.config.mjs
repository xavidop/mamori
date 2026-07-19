import { defineConfig } from "astro/config";
import sitemap from "@astrojs/sitemap";

// GitHub Pages project site: https://mamorigo.dev/
// The Pages workflow passes BASE_PATH (usually "/mamori"); locally it's "/".
const base = process.env.BASE_PATH || "/";

export default defineConfig({
  site: "https://xavidop.github.io",
  base,
  trailingSlash: "ignore",
  devToolbar: { enabled: false },
  build: { format: "directory" },
  integrations: [sitemap()],
  markdown: {
    shikiConfig: {
      theme: "github-light",
      wrap: false,
    },
  },
});
