import { createPinia } from "pinia";
import { createApp } from "vue";

import App from "./App.vue";
import { i18n } from "./shared/i18n";
import { router } from "./shared/router";
import { mountAfterInitialNavigation } from "./shared/routes";
import "./main.css";

const app = createApp(App).use(createPinia()).use(router).use(i18n);
void mountAfterInitialNavigation(router, () => app.mount("#app"));
