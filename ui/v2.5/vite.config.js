import path from "node:path";
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tsconfigPaths from "vite-tsconfig-paths";
import viteCompression from "vite-plugin-compression";

const sourcemap = process.env.VITE_APP_SOURCEMAPS === "true";

// public/sw.js removes the former UI config service worker from browsers
// that still have it; it must keep its name and stay uncompressed.
const serviceWorkerFile = "sw.js";

// https://vitejs.dev/config/
export default defineConfig(() => {
  const plugins = [
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
      // the service worker removal script must stay uncompressed
      filter: (file) =>
        /\.(js|json|css|svg|md)$/i.test(file) &&
        path.basename(file) !== serviceWorkerFile,
    }),
  ];

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
