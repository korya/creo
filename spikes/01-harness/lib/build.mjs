import { cp, readFile, writeFile, rm, readdir } from "node:fs/promises";
import { join } from "node:path";
import * as cheerio from "cheerio";

// Arm A: dist = verbatim copy. Arm B: dist = copy + content.json injected into [data-node-id] elements.
export async function build(src, dist, arm) {
  await rm(dist, { recursive: true, force: true });
  await cp(src, dist, { recursive: true });
  if (arm !== "B") return { injected: 0, orphanKeys: [] };

  let content = {};
  try {
    content = JSON.parse(await readFile(join(src, "content.json"), "utf8"));
  } catch {
    return { injected: 0, orphanKeys: [], error: "content.json missing or invalid" };
  }

  const usedKeys = new Set();
  let injected = 0;
  for (const file of await readdir(dist, { recursive: true })) {
    if (!file.endsWith(".html")) continue;
    const path = join(dist, file);
    const $ = cheerio.load(await readFile(path, "utf8"));
    $("[data-node-id]").each((_, el) => {
      const id = $(el).attr("data-node-id");
      const entry = content[id];
      if (!entry) return;
      usedKeys.add(id);
      if (entry.src !== undefined) $(el).attr("src", entry.src);
      if (entry.text !== undefined) $(el).text(entry.text);
      injected++;
    });
    await writeFile(path, $.html(), "utf8");
  }
  const orphanKeys = Object.keys(content).filter((k) => !usedKeys.has(k));
  return { injected, orphanKeys };
}
