import eslint from "@eslint/js";
import tseslint from "typescript-eslint";

// Match relative imports at any depth; the project has no source aliases.
const pageOwnedImports = {
  regex:
    "(^|/)(views(/|$)|shared/(router|fleet|localSlice)(\\.[^/]+)?$)|^vue-router(/|$)",
  message:
    "Keep routing and Fleet/local-slice state in page composition; use API modules and injected context/tracking callbacks.",
};

export default tseslint.config(
  eslint.configs.recommended,
  ...tseslint.configs.strictTypeChecked,
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
