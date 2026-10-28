import js from "@eslint/js";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";
import tseslint from "typescript-eslint";

export default tseslint.config(
  {
    ignores: ["dist", "src/api/generated"],
  },
  js.configs.recommended,
  ...tseslint.configs.strict,
  reactHooks.configs.flat.recommended,
  { languageOptions: { globals: { ...globals.browser } } },
  {
    files: ["e2e/**", "playwright.acceptance.config.ts"],
    languageOptions: { globals: { ...globals.node } },
  },
);
