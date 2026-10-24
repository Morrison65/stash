import { defineConfig } from "vitest/config";

// Unit tests for pure modules. Kept separate from vite.config.js so that the
// app's build plugins (legacy, compression, service worker) are not loaded.
export default defineConfig({
  test: {
    include: ["src/**/*.test.ts"],
    environment: "node",
  },
});
