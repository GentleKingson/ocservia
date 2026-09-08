<script setup lang="ts">
import { Activity, LogIn } from "@lucide/vue";
import type { AuthMethods } from "@ocservia/api-client";
import { onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";

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
  <main class="login-view">
    <section class="login-content" :aria-label="t('loginTitle')">
      <div class="login-brand">
        <Activity :size="24" /><span>{{ t("brand") }}</span>
      </div>
      <h1>{{ t("loginTitle") }}</h1>
      <p v-if="loading" role="status">{{ t("loading") }}</p>
      <p v-if="error" class="login-error" role="alert">{{ error }}</p>
      <form v-if="methods?.local" @submit.prevent="signInLocal">
        <label for="login-username">{{ t("loginUsername") }}</label>
        <input
          id="login-username"
          v-model="username"
          name="username"
          autocomplete="username"
          required
          :disabled="pending"
        />
        <label for="login-password">{{ t("loginPassword") }}</label>
        <input
          id="login-password"
          v-model="password"
          name="password"
          type="password"
          autocomplete="current-password"
          required
          :disabled="pending"
        />
        <button class="primary" type="submit" :disabled="pending">
          <LogIn :size="18" />{{
            t(pending ? "loginSubmitting" : "loginSubmit")
          }}
        </button>
      </form>
      <div v-if="methods?.local && methods.oidc" class="login-divider">
        {{ t("loginOr") }}
      </div>
      <button
        v-if="methods?.oidc && (methods.local || ssoStopped)"
        type="button"
        :disabled="pending"
        @click="signInSSO"
      >
        <LogIn :size="18" />{{ t("loginSSO") }}
      </button>
      <p v-if="methods?.oidc && !methods.local && !ssoStopped" role="status">
        {{ t("loginRedirecting") }}
      </p>
      <button v-if="!loading && !methods" type="button" @click="loadMethods">
        {{ t("loginRetry") }}
      </button>
    </section>
  </main>
</template>

<style scoped>
.login-view {
  min-height: 100vh;
  display: grid;
  place-items: center;
  padding: 32px 20px;
}
.login-content {
  width: 100%;
  max-width: 360px;
}
.login-brand {
  display: flex;
  align-items: center;
  gap: 10px;
  font-size: 20px;
  font-weight: 650;
  color: #176d47;
}
h1 {
  font-size: 24px;
  margin: 28px 0 24px;
}
form {
  display: grid;
  gap: 12px;
}
label {
  font-size: 14px;
  font-weight: 650;
}
input,
button {
  width: 100%;
  min-width: 0;
  min-height: 42px;
  border: 1px solid #bac7c2;
  border-radius: 5px;
  background: #fff;
  padding: 10px 12px;
  font: inherit;
}
button {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  cursor: pointer;
}
button.primary {
  margin-top: 8px;
  background: #176d47;
  border-color: #176d47;
  color: #fff;
}
button:disabled {
  opacity: 0.55;
  cursor: not-allowed;
}
.login-divider {
  display: flex;
  align-items: center;
  gap: 14px;
  margin: 24px 0;
  color: #5d6b66;
}
.login-divider::before,
.login-divider::after {
  content: "";
  flex: 1;
  height: 1px;
  background: #d4ddd9;
}
.login-error {
  color: #a12c36;
  overflow-wrap: anywhere;
}
</style>
