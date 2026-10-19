import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";

// The console API lives on the proxy's public data plane, not the admin port.
// In production one nginx serves this app and proxies /web/v1 to the same
// hostname, which is what keeps the session cookie same-origin.
const apiTarget = process.env.VITE_WEB_API_TARGET ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    port: 3334,
    proxy: {
      "/web": {
        target: apiTarget,
        changeOrigin: true,
      },
    },
  },
});
