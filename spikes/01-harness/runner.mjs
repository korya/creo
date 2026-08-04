import Anthropic from "@anthropic-ai/sdk";
import { readFile, writeFile, mkdir, appendFile, cp, rm } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import { makeTools } from "./lib/tools.mjs";
import { build } from "./lib/build.mjs";
import { screenshotSite } from "./lib/screenshot.mjs";

const MODEL = process.env.SPIKE_MODEL || "claude-sonnet-5";
const MAX_ITERATIONS = 40;

const SYSTEM_COMMON = `You are the build engine of a website builder for people who cannot code. You edit a static website in the workspace using the provided tools.

Rules:
- The site is plain static HTML/CSS/JS with relative paths only. No frameworks, no build tools, no external network resources (no CDN fonts or scripts, no remote images). For images, create local SVG placeholder files in assets/.
- Always inspect the current site (list_files, read_file) before editing an existing site.
- Scope discipline: change what was asked and nothing else. Do not restyle, rewrite, or reorganize unrelated parts of the site.
- If a request needs server-side functionality (payments, databases, accounts), do not attempt it. Explain in plain language that the site is a simple static site and suggest a realistic alternative (e.g. a phone number or email).
- Your final message is shown to a non-technical user: 1-3 short plain-language sentences about what changed. No file names, no code talk.`;

const SYSTEM_ARM_B = `

Structural conventions (mandatory):
- Every user-visible content element (headings, paragraphs, images, opening hours, contact details, quotes) carries a stable data-node-id attribute. Use short kebab-case ids (e.g. hero-heading, opening-hours, portrait-photo-1). Never rename an existing id.
- All user-visible text and image sources live in content.json at the workspace root, keyed by node id: {"hero-heading": {"text": "Welcome to Kastanja"}, "hero-image": {"src": "assets/hero.svg"}}. At build time each element's text or src is injected from content.json - keep the element's inner text in HTML as a short placeholder.
- Content changes (wording, hours, image swaps) belong in content.json. Structure and style changes belong in HTML/CSS.`;

function parseCli() {
  const { values } = parseArgs({
    options: {
      arm: { type: "string" },
      scenario: { type: "string" },
      from: { type: "string", default: "1" },
      to: { type: "string" },
    },
  });
  if (!["A", "B"].includes(values.arm) || !["s1-bakery", "s2-portfolio"].includes(values.scenario)) {
    console.error("usage: node runner.mjs --arm A|B --scenario s1-bakery|s2-portfolio [--from N] [--to N]");
    process.exit(1);
  }
  return values;
}

async function runAgent(client, system, prompt, workdir) {
  const tools = makeTools(workdir);
  const messages = [{ role: "user", content: prompt }];
  const usage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, turns: 0 };
  let finalText = "";

  for (let i = 0; i < MAX_ITERATIONS; i++) {
    const response = await client.messages.create({
      model: MODEL,
      max_tokens: 16000,
      system,
      tools: tools.defs,
      messages,
    });
    usage.turns++;
    usage.input += response.usage.input_tokens;
    usage.output += response.usage.output_tokens;
    usage.cacheRead += response.usage.cache_read_input_tokens ?? 0;
    usage.cacheWrite += response.usage.cache_creation_input_tokens ?? 0;

    finalText = response.content.filter((b) => b.type === "text").map((b) => b.text).join("\n");

    if (response.stop_reason === "pause_turn") {
      messages.push({ role: "assistant", content: response.content });
      continue;
    }
    if (response.stop_reason !== "tool_use") {
      if (response.stop_reason !== "end_turn") finalText += `\n[stop_reason: ${response.stop_reason}]`;
      break;
    }

    messages.push({ role: "assistant", content: response.content });
    const results = [];
    for (const block of response.content) {
      if (block.type !== "tool_use") continue;
      try {
        results.push({ type: "tool_result", tool_use_id: block.id, content: await tools.execute(block.name, block.input) });
      } catch (e) {
        results.push({ type: "tool_result", tool_use_id: block.id, content: `Error: ${e.message}`, is_error: true });
      }
    }
    messages.push({ role: "user", content: results });
  }
  return { finalText, usage, toolCalls: tools.stats.calls, writes: tools.stats.writes, writtenPaths: [...tools.stats.writtenPaths] };
}

// Arm B token-free path: resolve a content.json key by pattern and set it directly.
async function tryDeterministic(workdir, det) {
  let content;
  try {
    content = JSON.parse(await readFile(join(workdir, "content.json"), "utf8"));
  } catch {
    return { miss: "content.json missing or invalid" };
  }
  const re = new RegExp(det.keyPattern, "i");
  const key = Object.keys(content).find((k) => re.test(k));
  if (!key) return { miss: `no key matching /${det.keyPattern}/i in [${Object.keys(content).join(", ")}]` };
  if (det.requiresFile) {
    try {
      await readFile(join(workdir, det.requiresFile));
    } catch {
      return { miss: `required file ${det.requiresFile} does not exist (asset must be created by an agent first)` };
    }
  }
  if (det.text !== undefined) content[key] = { ...content[key], text: det.text };
  if (det.src !== undefined) content[key] = { ...content[key], src: det.src };
  await writeFile(join(workdir, "content.json"), JSON.stringify(content, null, 2), "utf8");
  return { key };
}

// Arm B convention check, logged per task (adherence is itself a measurement).
async function validateConventions(workdir) {
  const issues = [];
  try {
    JSON.parse(await readFile(join(workdir, "content.json"), "utf8"));
  } catch {
    issues.push("content.json missing or unparseable");
  }
  return issues;
}

const { arm, scenario, from, to } = parseCli();
const tasks = JSON.parse(await readFile(join("tasks", `${scenario}.json`), "utf8"));
const first = Number(from);
const last = to ? Number(to) : tasks.length;

const workdir = join("work", arm, scenario);
const resultsDir = join("results", arm, scenario);
await mkdir(workdir, { recursive: true });
await mkdir(resultsDir, { recursive: true });

const client = new Anthropic();
const system = arm === "B" ? SYSTEM_COMMON + SYSTEM_ARM_B : SYSTEM_COMMON;

for (const task of tasks) {
  if (task.id < first || task.id > last) continue;
  const t0 = Date.now();
  const entry = { arm, scenario, task: task.id, type: task.type, model: MODEL };
  console.log(`\n=== [${arm}/${scenario}] task ${task.id} (${task.type}) ===`);

  let prompt = task.prompt;
  if (task.type === "click") {
    if (arm === "B") {
      let ids = [];
      try {
        ids = Object.keys(JSON.parse(await readFile(join(workdir, "content.json"), "utf8")));
      } catch {}
      const re = new RegExp(task.clickPattern, "i");
      const hit = ids.find((k) => re.test(k));
      prompt = hit
        ? `The user clicked the element with data-node-id "${hit}" and said: ${task.prompt}`
        : `${task.prompt} (the user pointed at ${task.clickDescriptionA})`;
      entry.clickTarget = hit ?? "UNRESOLVED";
    } else {
      prompt = `${task.prompt} (the user is pointing at ${task.clickDescriptionA})`;
    }
  }

  let ranAgent = true;
  if (arm === "B" && task.deterministic) {
    const det = await tryDeterministic(workdir, task.deterministic);
    if (det.key) {
      entry.deterministic = { hit: det.key };
      entry.usage = { input: 0, output: 0, turns: 0 };
      entry.finalText = `(deterministic content edit on "${det.key}" - no LLM call)`;
      ranAgent = false;
      console.log(`deterministic hit: ${det.key}`);
    } else {
      entry.deterministic = { miss: det.miss };
      console.log(`deterministic MISS (${det.miss}) - falling back to agent`);
    }
  }

  if (ranAgent) {
    const r = await runAgent(client, system, prompt, workdir);
    Object.assign(entry, r);
  }

  if (arm === "B") entry.conventionIssues = await validateConventions(workdir);

  const dist = join(resultsDir, `task-${String(task.id).padStart(2, "0")}`, "site");
  const buildInfo = await build(workdir, dist, arm);
  entry.build = buildInfo;
  entry.screenshots = await screenshotSite(dist, join(resultsDir, `task-${String(task.id).padStart(2, "0")}`, "shots"));
  entry.seconds = Math.round((Date.now() - t0) / 10) / 100;

  await appendFile(join(resultsDir, "log.jsonl"), JSON.stringify(entry) + "\n");
  console.log(`done in ${entry.seconds}s | turns=${entry.usage?.turns ?? 0} in=${entry.usage?.input ?? 0} out=${entry.usage?.output ?? 0}`);
  console.log(`agent says: ${entry.finalText?.slice(0, 300) ?? ""}`);
}

console.log(`\nAll requested tasks finished for arm ${arm} / ${scenario}.`);
