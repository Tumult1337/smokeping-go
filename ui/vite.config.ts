import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import path from "node:path";

// Build output lands in ../internal/ui/dist so the Go binary can embed it via
// //go:embed all:dist (see internal/ui/ui.go).
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: path.resolve(__dirname, "../internal/ui/dist"),
    emptyOutDir: true,
  },
  server: {
    proxy: {
      "/api": "http://localhost:8080",
    },
  },
});
