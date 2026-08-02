import eslint from "@eslint/js";
import tseslint from "typescript-eslint";

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
    ignores: ["coverage/**", "src/api/generated/**"],
  },
);
