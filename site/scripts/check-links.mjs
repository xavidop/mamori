// Internal link checker for the built site (run after `astro build`).
//
// Walks every .html file under dist/, extracts internal <a href> links, and
// verifies each one resolves to a real page and, when it carries a #fragment,
// that the target page actually has an element with that id. Exits non-zero
// with a report if anything is broken, so CI fails on a broken doc link.
//
// External links (http/https/mailto/tel), and pure "#" fragments, are skipped:
// this checks the site's own internal wiring, not the reachability of the web.

import { readFileSync } from "node:fs";
import { readdir, stat } from "node:fs/promises";
import path from "node:path";
import process from "node:process";

const distDir = path.resolve(process.argv[2] || "dist");
// When the site is built with a base path (BASE_PATH=/mamori on GitHub Pages),
// root-relative hrefs are prefixed with it, but the dist file layout is not.
// Strip the base so root-relative links resolve to real files either way.
const basePrefix = (process.env.BASE_PATH || "").replace(/\/+$/, "");

async function walk(dir) {
  const out = [];
  for (const name of await readdir(dir)) {
    const full = path.join(dir, name);
    const s = await stat(full);
    if (s.isDirectory()) out.push(...(await walk(full)));
    else if (name.endsWith(".html")) out.push(full);
  }
  return out;
}

// Cache of file -> set of element ids, for fragment checks.
const idCache = new Map();
function idsOf(file) {
  if (idCache.has(file)) return idCache.get(file);
  let ids = new Set();
  try {
    const html = readFileSync(file, "utf8");
    for (const m of html.matchAll(/\bid="([^"]+)"/g)) ids.add(m[1]);
    for (const m of html.matchAll(/\bname="([^"]+)"/g)) ids.add(m[1]);
  } catch {
    /* missing file reported by the caller */
  }
  idCache.set(file, ids);
  return ids;
}

function fileExists(file) {
  try {
    readFileSync(file);
    return true;
  } catch {
    return false;
  }
}

// Resolve an href (as it appears in HTML at pageFile) to a dist file path.
// Returns { file, fragment } or null when the link is external / not checkable.
function resolveTarget(pageFile, href) {
  let link = href.trim();
  if (
    link === "" ||
    link.startsWith("http://") ||
    link.startsWith("https://") ||
    link.startsWith("mailto:") ||
    link.startsWith("tel:") ||
    link.startsWith("//") ||
    link.startsWith("data:") ||
    link.startsWith("javascript:")
  ) {
    return null;
  }

  let fragment = "";
  const hashAt = link.indexOf("#");
  if (hashAt >= 0) {
    fragment = link.slice(hashAt + 1);
    link = link.slice(0, hashAt);
  }
  link = link.split("?")[0];

  // Pure same-page fragment.
  if (link === "") return { file: pageFile, fragment };

  let targetPath;
  if (link.startsWith("/")) {
    let rooted = link;
    if (basePrefix && rooted.startsWith(basePrefix + "/")) rooted = rooted.slice(basePrefix.length);
    else if (basePrefix && rooted === basePrefix) rooted = "/";
    targetPath = path.join(distDir, rooted);
  } else {
    targetPath = path.resolve(path.dirname(pageFile), link);
  }

  // A directory-style URL (no file extension) maps to its index.html.
  if (!path.extname(targetPath)) {
    targetPath = path.join(targetPath, "index.html");
  }
  return { file: targetPath, fragment };
}

const pages = await walk(distDir);
const problems = [];

for (const page of pages) {
  const raw = readFileSync(page, "utf8");
  const rel = path.relative(distDir, page);
  // Drop <script> and <style> bodies so client-side templates that build
  // href="${...}" strings (e.g. the search box) are not mistaken for links.
  const html = raw
    .replace(/<script\b[^>]*>[\s\S]*?<\/script>/gi, "")
    .replace(/<style\b[^>]*>[\s\S]*?<\/style>/gi, "");
  for (const m of html.matchAll(/<a\b[^>]*?\shref="([^"]*)"/g)) {
    const target = resolveTarget(page, m[1]);
    if (!target) continue;
    if (!fileExists(target.file)) {
      problems.push(`${rel}  ->  ${m[1]}  (missing page)`);
      continue;
    }
    if (target.fragment && !idsOf(target.file).has(target.fragment)) {
      problems.push(`${rel}  ->  ${m[1]}  (missing #${target.fragment})`);
    }
  }
}

if (problems.length) {
  console.error(`\nBroken internal links (${problems.length}):`);
  for (const p of [...new Set(problems)].sort()) console.error("  " + p);
  console.error("");
  process.exit(1);
}
console.log(`Link check passed: ${pages.length} pages, no broken internal links.`);
