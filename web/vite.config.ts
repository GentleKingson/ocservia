import vue from "@vitejs/plugin-vue";
import { defineConfig } from "vite";

export default defineConfig({
  plugins: [vue()],
  server: {
    allowedHosts: ["web"],
    host: "0.0.0.0",
    port: 4173,
    proxy: {
      "/api": process.env.VITE_API_TARGET ?? "http://127.0.0.1:8080",
    },
  },
});
