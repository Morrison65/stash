import path from "path";
import { build as viteBuild, defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import legacy from "@vitejs/plugin-legacy";
import tsconfigPaths from "vite-tsconfig-paths";
import viteCompression from "vite-plugin-compression";

const nolegacy = process.env.VITE_APP_NOLEGACY === "true";
const sourcemap = process.env.VITE_APP_SOURCEMAPS === "true";

const serviceWorkerFile = "sw.js";

// Emits the service worker (src/sw/sw.ts) as a classic single-file script
// with a stable name at the build root (build/sw.js), next to index.html,
// so that its scope covers the whole app. See docs/architecture/ui-config.md.
function serviceWorkerPlugin() {
  let resolved;
  return {
    name: "stash-service-worker",
    apply: "build",
    configResolved(config) {
      resolved = config;
    },
    async closeBundle() {
      await viteBuild({
        configFile: false,
        root: resolved.root,
        logLevel: "warn",
        publicDir: false,
        define: {
          "process.env.NODE_ENV": JSON.stringify("production"),
        },
        build: {
          outDir: resolved.build.outDir,
          emptyOutDir: false,
          copyPublicDir: false,
          sourcemap: resolved.build.sourcemap,
          reportCompressedSize: false,
          minify: true,
          target: "es2019",
          lib: {
            entry: path.resolve(resolved.root, "src/sw/sw.ts"),
            formats: ["iife"],
            name: "stashServiceWorker",
            fileName: () => serviceWorkerFile,
          },
          rollupOptions: {
            output: { inlineDynamicImports: true },
          },
        },
      });
    },
  };
}

// https://vitejs.dev/config/
export default defineConfig(() => {
  let plugins = [
    react({
      babel: {
        compact: true,
      },
    }),
    tsconfigPaths(),
    viteCompression({
      algorithm: "gzip",
      deleteOriginFile: true,
      threshold: 0,
      // the service worker must stay uncompressed under a stable name
      filter: (file) =>
        /\.(js|json|css|svg|md)$/i.test(file) &&
        path.basename(file) !== serviceWorkerFile,
    }),
    serviceWorkerPlugin(),
  ];

  if (!nolegacy) {
    plugins = [...plugins, legacy()];
  }

  return {
    base: "",
    build: {
      outDir: "build",
      sourcemap: sourcemap,
      reportCompressedSize: false,
    },
    optimizeDeps: {
      entries: "src/index.tsx",
    },
    server: {
      port: 3000,
      cors: false,
    },
    publicDir: "public",
    assetsInclude: ["**/*.md"],
    plugins,
  };
});
