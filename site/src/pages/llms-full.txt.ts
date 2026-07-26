import type { APIRoute } from "astro";
import {
  collectDocs,
  absoluteUrl,
  LLMS_TITLE,
  LLMS_DESCRIPTION,
} from "../lib/docs-content";

// GET /llms-full.txt - the entire documentation concatenated into one Markdown
// file, so a coding agent can load everything in a single fetch.
export const GET: APIRoute = ({ site }) => {
  const docs = collectDocs();
  const header = [
    `# ${LLMS_TITLE} documentation`,
    "",
    `> ${LLMS_DESCRIPTION}`,
    "",
    "The entire mamori documentation, concatenated for agent consumption.",
    "",
  ].join("\n");

  const sections = docs.map((d) =>
    ["", "---", "", `> Source: ${absoluteUrl(d.path, site)}`, "", d.body, ""].join("\n"),
  );

  return new Response(header + sections.join("\n"), {
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
};
