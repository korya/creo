import { readFile, writeFile, mkdir, rm, readdir } from "node:fs/promises";
import { join, normalize, dirname, relative } from "node:path";

// All paths are confined to the workspace root; model-supplied paths are untrusted.
function resolveSafe(root, p) {
  const full = normalize(join(root, p));
  if (relative(root, full).startsWith("..")) throw new Error(`path escapes workspace: ${p}`);
  return full;
}

async function listRecursive(root, dir = root, acc = []) {
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    if (entry.name === "node_modules" || entry.name.startsWith(".")) continue;
    const full = join(dir, entry.name);
    if (entry.isDirectory()) await listRecursive(root, full, acc);
    else acc.push(relative(root, full));
  }
  return acc;
}

export function makeTools(root) {
  const defs = [
    {
      name: "list_files",
      description: "List every file in the site workspace (relative paths).",
      input_schema: { type: "object", properties: {}, required: [] },
    },
    {
      name: "read_file",
      description: "Read a file from the site workspace.",
      input_schema: {
        type: "object",
        properties: { path: { type: "string", description: "Relative path" } },
        required: ["path"],
      },
    },
    {
      name: "write_file",
      description: "Create or fully overwrite a file in the site workspace. Parent directories are created automatically.",
      input_schema: {
        type: "object",
        properties: {
          path: { type: "string", description: "Relative path" },
          content: { type: "string", description: "Full new file content" },
        },
        required: ["path", "content"],
      },
    },
    {
      name: "delete_file",
      description: "Delete a file from the site workspace.",
      input_schema: {
        type: "object",
        properties: { path: { type: "string", description: "Relative path" } },
        required: ["path"],
      },
    },
  ];

  const stats = { calls: 0, writes: 0, writtenPaths: new Set() };

  async function execute(name, input) {
    stats.calls++;
    switch (name) {
      case "list_files":
        return (await listRecursive(root)).sort().join("\n") || "(empty workspace)";
      case "read_file":
        return await readFile(resolveSafe(root, input.path), "utf8");
      case "write_file": {
        const full = resolveSafe(root, input.path);
        await mkdir(dirname(full), { recursive: true });
        await writeFile(full, input.content, "utf8");
        stats.writes++;
        stats.writtenPaths.add(input.path);
        return `wrote ${input.path} (${input.content.length} chars)`;
      }
      case "delete_file":
        await rm(resolveSafe(root, input.path));
        stats.writes++;
        stats.writtenPaths.add(input.path);
        return `deleted ${input.path}`;
      default:
        throw new Error(`unknown tool: ${name}`);
    }
  }

  return { defs, execute, stats };
}
