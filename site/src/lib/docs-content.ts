// Shared source for the /llms.txt and /llms-full.txt endpoints.
//
// These are rendered as ordinary Astro static endpoints (src/pages/*.txt.ts),
// so the files exist under `astro dev` AND `astro build` alike - unlike a
// build-only integration whose astro:build:done hook never runs in the dev
// server and 404s there. Every doc page's raw Markdown is read at build time
// via import.meta.glob so both endpoints render from one place.

export const LLMS_TITLE = "mamori";
export const LLMS_DESCRIPTION =
  "Typed, validated, watchable configuration and secrets for Go, loaded from a broad provider ecosystem (env, files, AWS, Vault, GCP, Azure, Kubernetes, databases, feature flags) and reconciled at runtime without a restart.";
export const LLMS_NOTES =
  "Auto-generated from the mamori documentation at https://mamorigo.dev/docs.";

// Every doc page, raw (frontmatter included), keyed by project-absolute path.
const rawPages = import.meta.glob("/src/pages/docs/**/*.md", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

export interface DocPage {
  /** Route-relative path, e.g. "/docs/server". */
  path: string;
  title: string;
  /** Markdown body with the frontmatter block stripped. */
  body: string;
}

const FRONTMATTER = /^---\r?\n([\s\S]*?)\r?\n---\r?\n?/;

function routeFor(fileKey: string): string {
  let p = fileKey.replace(/^\/src\/pages/, "").replace(/\.md$/, "");
  if (p.endsWith("/index")) p = p.slice(0, -"/index".length);
  return p || "/";
}

function titleFrom(frontmatter: string, body: string, route: string): string {
  const fm = frontmatter.match(/^title:\s*(.+?)\s*$/m);
  if (fm) return fm[1].replace(/^["']|["']$/g, "");
  const h1 = body.match(/^#\s+(.+?)\s*$/m);
  if (h1) return h1[1];
  return route.split("/").pop() || route;
}

// Pages that lead the index; everything else follows, ordered by title.
const ORDER = ["/docs", "/docs/quickstart"];

export function collectDocs(): DocPage[] {
  const pages: DocPage[] = Object.entries(rawPages).map(([key, raw]) => {
    const route = routeFor(key);
    const m = raw.match(FRONTMATTER);
    const frontmatter = m ? m[1] : "";
    const body = (m ? raw.slice(m[0].length) : raw).trim();
    return { path: route, title: titleFrom(frontmatter, body, route), body };
  });
  pages.sort((a, b) => {
    const ia = ORDER.indexOf(a.path);
    const ib = ORDER.indexOf(b.path);
    if (ia !== -1 || ib !== -1) {
      return (ia === -1 ? Infinity : ia) - (ib === -1 ? Infinity : ib);
    }
    return a.title.localeCompare(b.title);
  });
  return pages;
}

// Absolute URL honouring the configured base and site; falls back to a
// root-relative path when the site is not configured.
export function absoluteUrl(route: string, site: URL | undefined): string {
  const base = import.meta.env.BASE_URL || "/";
  const joined = (base.replace(/\/$/, "") + route).replace(/\/{2,}/g, "/");
  return site ? new URL(joined, site).href : joined;
}
