import eslint from "@eslint/js";
import pluginVue from "eslint-plugin-vue";
import tseslint from "typescript-eslint";

// Match relative imports at any depth and the `@/` source alias.
const pageOwnedImports = {
  regex:
    "(^|/)(views(/|$)|shared/(router|fleet|localSlice)(\\.[^/]+)?$)|^vue-router(/|$)",
  message:
    "Keep routing and Fleet/local-slice state in page composition; use API modules and injected context/tracking callbacks.",
};

export default tseslint.config(
  eslint.configs.recommended,
  ...tseslint.configs.strictTypeChecked,
  ...pluginVue.configs["flat/essential"],
  {
    files: [
      "src/**/*.ts",
      "test/**/*.ts",
      "e2e/**/*.ts",
      "playwright.config.ts",
    ],
    languageOptions: {
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
  },
  {
    // The Vue parser owns the template; <script lang="ts"> goes to the
    // TypeScript parser with the same project service as .ts sources.
    files: ["src/**/*.vue"],
    languageOptions: {
      parserOptions: {
        parser: tseslint.parser,
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
        extraFileExtensions: [".vue"],
      },
    },
  },
  {
    // Type checking (vue-tsc) owns undefined names, as for .ts sources.
    ...tseslint.configs.eslintRecommended,
    files: ["src/**/*.vue"],
  },
  {
    // shadcn-vue primitives keep their upstream single-word names.
    files: ["src/components/ui/**/*.vue"],
    rules: { "vue/multi-word-component-names": "off" },
  },
  {
    files: ["test/**/*.mjs"],
    ...tseslint.configs.disableTypeChecked,
  },
  {
    files: ["src/api/**/*.ts"],
    rules: {
      "no-restricted-imports": [
        "error",
        {
          patterns: [
            pageOwnedImports,
            {
              regex: "(^|/)features(/|$)",
              message:
                "API modules adapt requests; keep workflows in features and depend on generated clients, domain APIs or Workspace instead.",
            },
          ],
        },
      ],
    },
  },
  {
    files: [
      "src/features/configuration/**/*.ts",
      "src/features/certificates/**/*.ts",
    ],
    rules: {
      "no-restricted-imports": ["error", { patterns: [pageOwnedImports] }],
    },
  },
  {
    ignores: ["coverage/**", "src/api/generated/**"],
  },
);
