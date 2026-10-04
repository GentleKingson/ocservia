import { createPinia } from "pinia";
import { createApp } from "vue";

import App from "./App.vue";
import { i18n } from "./shared/i18n";
import { router } from "./shared/router";
import "./main.css";

const app = createApp(App).use(createPinia()).use(router).use(i18n);
// Mount after the initial navigation so the shell never renders for /login.
void router.isReady().then(() => app.mount("#app"));
