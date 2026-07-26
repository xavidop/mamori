import { defineConfig } from "astro/config";
import sitemap from "@astrojs/sitemap";
import { visit } from "unist-util-visit";

// GitHub Pages project site: https://mamorigo.dev/
// The Pages workflow passes BASE_PATH (usually "/mamori"); locally it's "/".
const base = process.env.BASE_PATH || "/";

// remarkMermaid turns ```mermaid fenced code blocks into a <pre class="mermaid">
// container that the client-side mermaid renderer (see DocsLayout.astro) picks
// up, instead of letting Shiki syntax-highlight the diagram source as code. The
// diagram text is HTML-escaped so characters like the --> arrow survive as
// textContent for mermaid to parse.
function remarkMermaid() {
  return (tree) => {
    visit(tree, "code", (node, index, parent) => {
      if (node.lang !== "mermaid" || !parent || typeof index !== "number") return;
      const escaped = String(node.value)
        .replace(/&/g, "&amp;")
        .replace(/</g, "&lt;")
        .replace(/>/g, "&gt;");
      parent.children[index] = {
        type: "html",
        value: `<pre class="mermaid">${escaped}</pre>`,
      };
    });
  };
}

export default defineConfig({
  site: "https://xavidop.github.io",
  base,
  // Internal doc links are root-relative (/docs/...), so they resolve the same
  // with or without a trailing slash; "ignore" lets the dev server serve both
  // forms rather than 404-ing bare URLs.
  trailingSlash: "ignore",
  devToolbar: { enabled: false },
  build: { format: "directory" },
  integrations: [sitemap()],
  markdown: {
    remarkPlugins: [remarkMermaid],
    shikiConfig: {
      theme: "github-light",
      wrap: false,
    },
  },
});
