import type { APIRoute } from "astro";
import {
  collectDocs,
  absoluteUrl,
  LLMS_TITLE,
  LLMS_DESCRIPTION,
  LLMS_NOTES,
} from "../lib/docs-content";

// GET /llms.txt - a short, link-first index of the documentation, following
// the llmstxt.org convention, for a coding agent to navigate.
export const GET: APIRoute = ({ site }) => {
  const docs = collectDocs();
  const body = [
    `# ${LLMS_TITLE}`,
    "",
    `> ${LLMS_DESCRIPTION}`,
    "",
    LLMS_NOTES,
    "",
    "## Documentation",
    "",
    ...docs.map((d) => `- [${d.title}](${absoluteUrl(d.path, site)})`),
    "",
    "## Full text",
    "",
    `- [Complete documentation](${absoluteUrl("/llms-full.txt", site)}): every page above concatenated into a single Markdown file.`,
    "",
  ].join("\n");

  return new Response(body, {
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
};
