// Guards the legacy/Tailwind coexistence while Tailwind scans all of src.
// A legacy class name that is also a Tailwind utility would gain utility
// declarations in the higher `utilities` layer. Delete this test together
// with the legacy layer (UI migration cleanup).
import { globSync, readFileSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { dirname, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
import { compile } from "tailwindcss";
import { expect, it } from "vitest";

const web = join(dirname(fileURLToPath(import.meta.url)), "..");
const tailwind = join(web, "node_modules/tailwindcss");

// All of src/components is skipped: it is utility-only and must not use
// legacy classes. Templates outside it are listed here once migrated; their
// class tokens are intentional utilities.
const migrated = new Set([
  "src/App.vue",
  "src/views/LoginView.vue",
  "src/views/NodesView.vue",
  "src/views/SettingsView.vue",
]);

function legacyClassNames(): Set<string> {
  const names = new Set<string>();
  const add = (value: string) => {
    for (const token of value.split(/\s+/))
      if (/^[a-z][a-z0-9_-]*$/.test(token)) names.add(token);
  };
  const styles = readFileSync(join(web, "src/styles.css"), "utf8");
  for (const match of styles.matchAll(/\.([a-zA-Z_][\w-]*)/g))
    add(match[1] ?? "");
  for (const file of globSync("src/**/*.vue", { cwd: web })) {
    const path = relative(web, join(web, file)).replaceAll("\\", "/");
    if (path.startsWith("src/components/") || migrated.has(path)) continue;
    const source = readFileSync(join(web, path), "utf8");
    for (const match of source.matchAll(/\sclass="([^"]*)"/g))
      add(match[1] ?? "");
    // Dynamic bindings: string literals and object keys.
    for (const match of source.matchAll(/\s:class="([^"]*)"/g)) {
      const binding = match[1] ?? "";
      for (const literal of binding.matchAll(/'([^']*)'|`([^`]*)`/g))
        add(literal[1] ?? literal[2] ?? "");
      for (const key of binding.matchAll(/([a-z][\w-]*)\s*:/g))
        add(key[1] ?? "");
    }
  }
  return names;
}

async function utilityClassNames(names: string[]): Promise<string[]> {
  const compiler = await compile(
    readFileSync(join(web, "src/main.css"), "utf8"),
    {
      base: join(web, "src"),
      loadStylesheet: async (id, base) => ({
        base,
        path: id,
        content: id.startsWith("tailwindcss/")
          ? await readFile(
              join(tailwind, id.slice("tailwindcss/".length)),
              "utf8",
            )
          : "",
      }),
    },
  );
  const css = compiler.build(names);
  return names.filter((name) => new RegExp(`\\.${name}(?![\\w-])`).test(css));
}

it("detects utility names with the project Tailwind configuration", async () => {
  expect(await utilityClassNames(["flex", "bg-card", "brand"])).toEqual([
    "flex",
    "bg-card",
  ]);
});

it("keeps legacy class names out of the Tailwind utility namespace", async () => {
  const names = [...legacyClassNames()];
  expect(names.length).toBeGreaterThan(100);
  expect(await utilityClassNames(names)).toEqual([]);
});
