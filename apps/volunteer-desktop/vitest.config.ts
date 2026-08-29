import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import path from "path";

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
      "@tauri-apps/api/core": path.resolve(
        __dirname,
        "./src/__mocks__/@tauri-apps/api/core.ts"
      ),
      "@tauri-apps/api/event": path.resolve(
        __dirname,
        "./src/__mocks__/@tauri-apps/api/event.ts"
      ),
    },
  },
  test: {
    globals: true,
    environment: "jsdom",
    setupFiles: ["./src/__tests__/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
