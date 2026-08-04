import { createServer } from "node:http";
import { readFile, readdir, mkdir } from "node:fs/promises";
import { join, extname, normalize } from "node:path";
import { chromium } from "playwright";

const MIME = {
  ".html": "text/html", ".css": "text/css", ".js": "text/javascript",
  ".json": "application/json", ".svg": "image/svg+xml", ".png": "image/png",
  ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
  ".woff2": "font/woff2", ".ico": "image/x-icon",
};

function serve(dir) {
  const server = createServer(async (req, res) => {
    try {
      let p = decodeURIComponent(new URL(req.url, "http://x").pathname);
      if (p.endsWith("/")) p += "index.html";
      const full = normalize(join(dir, p));
      if (!full.startsWith(normalize(dir))) throw new Error("traversal");
      const body = await readFile(full);
      res.writeHead(200, { "content-type": MIME[extname(full)] ?? "application/octet-stream" });
      res.end(body);
    } catch {
      res.writeHead(404).end("not found");
    }
  });
  return new Promise((resolve) => server.listen(0, () => resolve(server)));
}

// Screenshots every top-level .html page at desktop and mobile widths.
export async function screenshotSite(dir, outDir) {
  await mkdir(outDir, { recursive: true });
  const pages = (await readdir(dir)).filter((f) => f.endsWith(".html"));
  if (pages.length === 0) return [];

  const server = await serve(dir);
  const port = server.address().port;
  const browser = await chromium.launch();
  const shots = [];
  try {
    for (const [label, viewport] of [["desktop", { width: 1280, height: 900 }], ["mobile", { width: 390, height: 844 }]]) {
      const ctx = await browser.newContext({ viewport });
      const page = await ctx.newPage();
      for (const file of pages) {
        const out = join(outDir, `${file.replace(/\.html$/, "")}.${label}.png`);
        try {
          await page.goto(`http://127.0.0.1:${port}/${file}`, { waitUntil: "networkidle", timeout: 15000 });
          await page.screenshot({ path: out, fullPage: true });
          shots.push(out);
        } catch (e) {
          shots.push(`FAILED ${file} (${label}): ${e.message}`);
        }
      }
      await ctx.close();
    }
  } finally {
    await browser.close();
    server.close();
  }
  return shots;
}
