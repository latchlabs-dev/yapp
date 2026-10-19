import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";

// API proxy target is overridable so a local dev UI can be pointed at a remote
// deployment (e.g. VITE_API_TARGET=http://host:3333 VITE_API_STRIP_PREFIX=false
// routes through that deployment's own nginx, which strips /api itself).
const apiTarget = process.env.VITE_API_TARGET ?? "http://localhost:8081";
const stripApiPrefix = process.env.VITE_API_STRIP_PREFIX !== "false";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    port: 3333,
    proxy: {
      "/api": {
        target: apiTarget,
        changeOrigin: true,
        rewrite: stripApiPrefix ? (p) => p.replace(/^\/api/, "") : undefined,
      },
    },
  },
});
