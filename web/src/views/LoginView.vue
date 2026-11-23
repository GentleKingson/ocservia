<script setup lang="ts">
import { Activity, LogIn } from "@lucide/vue";
import { ref } from "vue";
import { useI18n } from "vue-i18n";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

import { loginLocal } from "../api/platform";
import { retryAfterSeconds } from "../shared/login";

const { t } = useI18n();
const username = ref("");
const password = ref("");
const pending = ref(false);
const error = ref("");

async function signInLocal(): Promise<void> {
  if (pending.value) return;
  pending.value = true;
  error.value = "";
  try {
    const request = loginLocal(username.value, password.value);
    password.value = "";
    const response = await request;
    if (response.ok) {
      window.location.replace("/");
      return;
    }
    if (response.status === 429) {
      const seconds = retryAfterSeconds(response.headers.get("Retry-After"));
      error.value =
        seconds === undefined
          ? t("loginRateLimited")
          : t("loginRetryAfter", { seconds });
    } else {
      error.value = t(
        response.status === 401 ? "loginInvalid" : "loginUnavailable",
      );
    }
  } catch {
    error.value = t("loginUnavailable");
  } finally {
    password.value = "";
    pending.value = false;
  }
}
</script>

<template>
  <main
    class="grid min-h-screen content-start justify-items-center px-5 pt-[18vh] pb-8"
  >
    <section
      class="bg-card border-border w-full max-w-[400px] rounded-xl border p-6 shadow-xs"
      :aria-label="t('loginTitle')"
    >
      <div
        class="text-foreground flex items-center gap-2.5 text-xl font-semibold"
      >
        <span
          class="from-primary/80 to-primary text-primary-foreground flex size-9 items-center justify-center rounded-[10px] bg-linear-to-b shadow-xs"
        >
          <Activity class="size-5" stroke-width="2.4" aria-hidden="true" />
        </span>
        <span>{{ t("brand") }}</span>
      </div>
      <h1
        class="text-foreground mt-7 mb-6 text-2xl font-semibold tracking-tight"
      >
        {{ t("loginTitle") }}
      </h1>
      <p
        v-if="error"
        class="border-destructive/30 bg-destructive/5 text-destructive mb-4 rounded-md border px-3 py-2 text-sm wrap-anywhere"
        role="alert"
      >
        {{ error }}
      </p>
      <form class="grid gap-3" @submit.prevent="signInLocal">
        <Label for="login-username">{{ t("loginUsername") }}</Label>
        <Input
          id="login-username"
          v-model="username"
          class="h-10"
          name="username"
          autocomplete="username"
          required
          :disabled="pending"
        />
        <Label for="login-password">{{ t("loginPassword") }}</Label>
        <Input
          id="login-password"
          v-model="password"
          class="h-10"
          name="password"
          type="password"
          autocomplete="current-password"
          required
          :disabled="pending"
        />
        <Button class="mt-2 h-10 w-full" type="submit" :disabled="pending">
          <LogIn aria-hidden="true" />{{
            t(pending ? "loginSubmitting" : "loginSubmit")
          }}
        </Button>
      </form>
    </section>
  </main>
</template>
