import vue from "@vitejs/plugin-vue";
import { defineConfig } from "vite";

const apiTarget = process.env.VITE_API_TARGET ?? "http://127.0.0.1:8080";
const devAuthToken = process.env.VITE_DEV_AUTH_TOKEN;

export default defineConfig({
  plugins: [
    vue(),
    {
      name: "reject-build-dev-auth-token",
      configResolved(config) {
        if (
          config.command === "build" &&
          (process.env.VITE_DEV_AUTH_TOKEN || config.env.VITE_DEV_AUTH_TOKEN)
        ) {
          throw new Error("VITE_DEV_AUTH_TOKEN must not be set for builds");
        }
      },
    },
  ],
  server: {
    allowedHosts: ["web"],
    host: "0.0.0.0",
    port: 4173,
    proxy: {
      "/api": {
        target: apiTarget,
        ...(devAuthToken
          ? { headers: { Authorization: `Bearer ${devAuthToken}` } }
          : {}),
      },
    },
  },
});
