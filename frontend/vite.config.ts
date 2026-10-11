import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

// https://vite.dev/config/
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    proxy: {
      "^/api/nodes/[^/]+/terminal-sessions/[^/]+/connect": {
        target: "https://127.0.0.1:8443",
        ws: true,
        secure: false,
        changeOrigin: false,
      },
      "/api": {
        target: "https://127.0.0.1:8443",
        changeOrigin: true,
        secure: false,
      },
    },
  },
  build: {
    outDir: "../internal/control/web/dist",
    emptyOutDir: true,
  },
});
