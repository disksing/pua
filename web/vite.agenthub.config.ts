import { cpSync, mkdirSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { svelte } from "@sveltejs/vite-plugin-svelte";
import { defineConfig } from "vite";

const webRoot = fileURLToPath(new URL(".", import.meta.url));
const repositoryRoot = resolve(webRoot, "..");
const frontendRoot = resolve(repositoryRoot, "agenthub/frontend");
const outputRoot = resolve(frontendRoot, "dist/client");

export default defineConfig({
  root: frontendRoot,
  base: "/agenthub/",
  publicDir: resolve(frontendRoot, "public"),
  plugins: [
    {
      name: "agenthub-svelte-entry",
      enforce: "pre",
      async resolveId(id, importer) {
        if (id === "/agenthub/assets/agenthub-app.js") return resolve(webRoot, "src/agenthub/entry.ts");
        if ((id === "svelte" || id.startsWith("svelte/")) && !importer?.includes("/node_modules/svelte/")) {
          return this.resolve(id, resolve(webRoot, "src/agenthub/entry.ts"), { skipSelf: true });
        }
        return null;
      },
    },
    svelte({ configFile: resolve(webRoot, "svelte.config.js") }),
    {
      name: "agenthub-browser-vendors",
      closeBundle() {
        const vendorRoot = resolve(outputRoot, "vendor");
        mkdirSync(vendorRoot, { recursive: true });
        for (const name of ["lucide", "dompurify", "marked"]) {
          cpSync(resolve(webRoot, "static/vendor", name), resolve(vendorRoot, name), { recursive: true });
        }
      },
    },
  ],
  build: {
    emptyOutDir: true,
    outDir: outputRoot,
    sourcemap: false,
    rollupOptions: {
      input: resolve(frontendRoot, "index.html"),
      output: {
        entryFileNames: "assets/agenthub-app.js",
        chunkFileNames: "assets/agenthub-[name].js",
        assetFileNames: (asset) => asset.names.some((name) => name.endsWith(".css")) ? "assets/agenthub-app.css" : "assets/[name][extname]",
      },
    },
  },
  server: {
    host: "127.0.0.1",
    proxy: {
      "/agenthub/v1": "http://127.0.0.1:4646",
    },
  },
  optimizeDeps: { exclude: ["svelte"] },
});
