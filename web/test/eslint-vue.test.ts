import { ESLint } from "eslint";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

// Lints in-memory SFC source through the real eslint.config.ts, so the check
// fails if .vue files stop reaching the Vue parser or its essential rules. The
// source borrows an existing component path (never written) because typed
// linting accepts only files in the TypeScript project.
const eslint = new ESLint({
  cwd: fileURLToPath(new URL("..", import.meta.url)),
});

async function ruleIds(source: string): Promise<(string | null)[]> {
  const [result] = await eslint.lintText(source, {
    filePath: "src/components/common/CopyButton.vue",
  });
  return (
    result?.messages.map((message) => message.ruleId ?? message.message) ?? [
      "no result",
    ]
  );
}

describe("Vue SFC lint", () => {
  it("accepts a typed script setup component", async () => {
    expect(
      await ruleIds(`<script setup lang="ts">
const props = defineProps<{ items: { id: string; label: string }[] }>();
const count = defineModel<number>({ required: true });
</script>

<template>
  <ul>
    <li v-for="item in props.items" :key="item.id">{{ item.label }}</li>
  </ul>
  <button type="button" @click="count += 1">{{ count }}</button>
</template>
`),
    ).toEqual([]);
  }, 30_000);

  it("reports prop mutation and template errors", async () => {
    const ids = await ruleIds(`<script setup lang="ts">
const props = defineProps<{ open: boolean; items: string[] }>();
function close(): void {
  props.open = false;
}
</script>

<template>
  <ul v-if>
    <li v-for="item in props.items" v-if="props.open">{{ item }}</li>
  </ul>
  <button type="button" @click="close">x</button>
</template>
`);
    expect(ids).toEqual(
      expect.arrayContaining([
        "vue/no-mutating-props",
        "vue/valid-v-if",
        "vue/require-v-for-key",
        "vue/no-use-v-if-with-v-for",
      ]),
    );
  }, 30_000);

  it("shares extraFileExtensions across typed .ts and .vue configs", async () => {
    const readExtraFileExtensions = async (
      filePath: string,
    ): Promise<unknown> => {
      const config: unknown = await eslint.calculateConfigForFile(filePath);
      if (typeof config !== "object" || config === null) return undefined;
      const languageOptions = (config as { languageOptions?: unknown })
        .languageOptions;
      if (typeof languageOptions !== "object" || languageOptions === null)
        return undefined;
      const parserOptions = (languageOptions as { parserOptions?: unknown })
        .parserOptions;
      if (typeof parserOptions !== "object" || parserOptions === null)
        return undefined;
      return (parserOptions as { extraFileExtensions?: unknown })
        .extraFileExtensions;
    };

    const vueExtensions = await readExtraFileExtensions(
      "src/components/common/CopyButton.vue",
    );
    expect(vueExtensions).toEqual([".vue"]);
    for (const filePath of [
      "src/main.ts",
      "test/eslint-vue.test.ts",
      "e2e/login.spec.ts",
      "vite.config.ts",
    ]) {
      expect(await readExtraFileExtensions(filePath), filePath).toEqual(
        vueExtensions,
      );
    }
  });
});
