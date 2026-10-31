import eslint from "@eslint/js";
import { defineConfig } from "eslint/config";
import pluginVue from "eslint-plugin-vue";
import tseslint from "typescript-eslint";

// Match relative imports at any depth and the `@/` source alias.
const pageOwnedImports = {
  regex:
    "(^|/)(views(/|$)|shared/(router|fleet|development-simulation)(\\.[^/]+)?$)|^vue-router(/|$)",
  message:
    "Keep routing and Fleet/development-simulation state in page composition; use API modules and injected context/tracking callbacks.",
};

export default defineConfig(
  eslint.configs.recommended,
  ...tseslint.configs.strictTypeChecked,
  ...pluginVue.configs["flat/essential"],
  {
    files: ["src/**/*.ts", "test/**/*.ts", "e2e/**/*.ts", "*.config.ts"],
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
    // The root script invocation uses --config web/eslint.config.ts from the
    // repository root, so these patterns are relative to that working directory.
    files: [
      "scripts/generate-controller-release-manifest.mjs",
      "scripts/redact-release-upgrade-log.mjs",
      "scripts/release-business-browser.mjs",
      "scripts/release-upgrade-oidc-fixture.mjs",
      "scripts/test-release-upgrade.mjs",
    ],
    ...tseslint.configs.disableTypeChecked,
    languageOptions: {
      sourceType: "module",
      globals: {
        process: "readonly",
        console: "readonly",
        Buffer: "readonly",
        URL: "readonly",
        URLSearchParams: "readonly",
        setTimeout: "readonly",
        clearTimeout: "readonly",
      },
    },
  },
  {
    // Playwright evaluate callbacks in this harness run in the browser.
    files: ["scripts/release-business-browser.mjs"],
    languageOptions: {
      globals: {
        document: "readonly",
        window: "readonly",
        sessionStorage: "readonly",
        Image: "readonly",
      },
    },
  },
  {
    files: [".github/actions/build-cache-credentials/index.js"],
    ...tseslint.configs.disableTypeChecked,
    languageOptions: {
      sourceType: "commonjs",
      globals: { process: "readonly", console: "readonly" },
    },
    rules: {
      ...tseslint.configs.disableTypeChecked.rules,
      "@typescript-eslint/no-require-imports": "off",
    },
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
