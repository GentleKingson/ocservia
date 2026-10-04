<script setup lang="ts">
import { Activity, LogIn } from "@lucide/vue";
import type { AuthMethods } from "@ocservia/api-client";
import { onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

import {
  hasOIDCLoginAttempt,
  startOIDCLoginAttempt,
  retryAfterSeconds,
} from "../shared/login";

const { t } = useI18n();
const methods = ref<AuthMethods>();
const username = ref("");
const password = ref("");
const pending = ref(false);
const loading = ref(true);
const error = ref("");
const ssoStopped = ref(
  hasOIDCLoginAttempt() ||
    new URLSearchParams(window.location.search).get("auth") === "failed",
);

function signInSSO(): void {
  password.value = "";
  startOIDCLoginAttempt();
  ssoStopped.value = false;
  window.location.assign("/api/v1/auth/login");
}

async function loadMethods(): Promise<void> {
  loading.value = true;
  error.value = "";
  try {
    const response = await fetch("/api/v1/auth/methods", {
      credentials: "same-origin",
      cache: "no-store",
    });
    if (!response.ok) throw new Error("Authentication unavailable");
    const data = (await response.json()) as AuthMethods;
    if (
      typeof data.local !== "boolean" ||
      typeof data.oidc !== "boolean" ||
      (!data.local && !data.oidc)
    )
      throw new Error("Authentication unavailable");
    methods.value = data;
    if (data.oidc && ssoStopped.value) error.value = t("loginSSOFailed");
    if (!data.local && data.oidc && !ssoStopped.value) signInSSO();
  } catch {
    error.value = t("loginUnavailable");
  } finally {
    loading.value = false;
  }
}

async function signInLocal(): Promise<void> {
  if (pending.value) return;
  pending.value = true;
  error.value = "";
  try {
    // Do not use authenticatedFetch: rejected credentials belong to this form.
    const request = fetch("/api/v1/auth/login", {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        username: username.value,
        password: password.value,
      }),
    });
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

onMounted(loadMethods);
</script>

<template>
  <main class="grid min-h-screen place-items-center px-5 py-8">
    <section class="w-full max-w-[360px]" :aria-label="t('loginTitle')">
      <div class="text-primary flex items-center gap-2.5 text-xl font-semibold">
        <Activity :size="24" aria-hidden="true" /><span>{{ t("brand") }}</span>
      </div>
      <h1 class="text-foreground mt-7 mb-6 text-2xl font-semibold">
        {{ t("loginTitle") }}
      </h1>
      <p v-if="loading" class="text-muted-foreground text-sm" role="status">
        {{ t("loading") }}
      </p>
      <p
        v-if="error"
        class="border-destructive/30 bg-destructive/5 text-destructive mb-4 rounded-md border px-3 py-2 text-sm wrap-anywhere"
        role="alert"
      >
        {{ error }}
      </p>
      <form
        v-if="methods?.local"
        class="grid gap-3"
        @submit.prevent="signInLocal"
      >
        <Label for="login-username">{{ t("loginUsername") }}</Label>
        <Input
          id="login-username"
          v-model="username"
          class="bg-card h-10"
          name="username"
          autocomplete="username"
          required
          :disabled="pending"
        />
        <Label for="login-password">{{ t("loginPassword") }}</Label>
        <Input
          id="login-password"
          v-model="password"
          class="bg-card h-10"
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
      <div
        v-if="methods?.local && methods.oidc"
        class="text-muted-foreground before:bg-border after:bg-border my-6 flex items-center gap-3.5 text-sm before:h-px before:flex-1 after:h-px after:flex-1"
      >
        {{ t("loginOr") }}
      </div>
      <Button
        v-if="methods?.oidc && (methods.local || ssoStopped)"
        class="h-10 w-full"
        variant="outline"
        type="button"
        :disabled="pending"
        @click="signInSSO"
      >
        <LogIn aria-hidden="true" />{{ t("loginSSO") }}
      </Button>
      <p
        v-if="methods?.oidc && !methods.local && !ssoStopped"
        class="text-muted-foreground text-sm"
        role="status"
      >
        {{ t("loginRedirecting") }}
      </p>
      <Button
        v-if="!loading && !methods"
        class="h-10 w-full"
        variant="outline"
        type="button"
        @click="loadMethods"
      >
        {{ t("loginRetry") }}
      </Button>
    </section>
  </main>
</template>
