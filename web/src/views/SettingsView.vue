<script setup lang="ts">
import { CheckCircle2, CircleAlert } from "@lucide/vue";
import type { BuildInfo, Workspace } from "@/api/models";
import { ResponseError } from "@/api/transport";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { onBeforeUnmount, onMounted, ref } from "vue";

import DataState from "../components/common/DataState.vue";
import PageHeader from "../components/common/PageHeader.vue";
import { getWorkspace } from "../api/workspace";
import { changeLocalPassword, getVersion } from "../api/platform";
import { useReadinessStore } from "../shared/readiness";

const readiness = useReadinessStore();
const workspace = ref<Workspace>();
const buildInfo = ref<BuildInfo>();
const loading = ref(true);
const unavailable = ref(false);
const buildUnavailable = ref(false);
let loadSequence = 0;
const currentPassword = ref("");
const newPassword = ref("");
const confirmNewPassword = ref("");
const changingPassword = ref(false);
const passwordError = ref("");

async function submitPassword(): Promise<void> {
  if (changingPassword.value) return;
  if (newPassword.value !== confirmNewPassword.value) {
    passwordError.value = "passwordConfirmationMismatch";
    return;
  }
  changingPassword.value = true;
  passwordError.value = "";
  try {
    const request = changeLocalPassword(
      currentPassword.value,
      newPassword.value,
    );
    currentPassword.value = newPassword.value = confirmNewPassword.value = "";
    await request;
    window.location.replace("/login");
  } catch (error) {
    const status = error instanceof ResponseError ? error.response.status : 0;
    switch (status) {
      case 400:
        passwordError.value = "passwordChangePolicy";
        break;
      case 401:
      case 429:
        passwordError.value = "passwordChangeRejected";
        break;
      case 403:
        passwordError.value = "passwordChangeForbidden";
        break;
      default:
        passwordError.value = "passwordChangeUnavailable";
    }
  } finally {
    changingPassword.value = false;
  }
}

async function loadWorkspace(): Promise<void> {
  const sequence = ++loadSequence;
  loading.value = true;
  unavailable.value = false;
  buildUnavailable.value = false;
  buildInfo.value = undefined;
  try {
    const [workspaceResult, buildResult] = await Promise.all([
      getWorkspace(),
      getVersion().then(
        (value) => ({ value, unavailable: false }),
        () => ({ value: undefined, unavailable: true }),
      ),
    ]);
    if (sequence !== loadSequence) return;
    workspace.value = workspaceResult;
    buildInfo.value = buildResult.value;
    buildUnavailable.value = buildResult.unavailable;
  } catch {
    if (sequence !== loadSequence) return;
    workspace.value = undefined;
    unavailable.value = true;
  } finally {
    if (sequence === loadSequence) loading.value = false;
  }
}

onMounted(() => {
  void loadWorkspace();
});
onBeforeUnmount(() => {
  loadSequence++;
});
</script>

<template>
  <main>
    <PageHeader :eyebrow="$t('platform')" :title="$t('settings')" />
    <DataState v-if="loading" kind="loading" :message="$t('loading')" />
    <DataState
      v-else-if="unavailable"
      kind="error"
      :message="$t('noWorkspace')"
    />
    <div v-else class="grid gap-4 md:grid-cols-2">
      <section
        class="bg-card border-border min-w-0 rounded-xl border p-5 shadow-xs md:col-span-2"
        aria-labelledby="settings-change-password"
      >
        <h2 id="settings-change-password" class="m-0 text-sm font-semibold">
          {{ $t("changePassword") }}
        </h2>
        <p class="text-muted-foreground mt-2 mb-4 text-sm">
          {{ $t("passwordChangeSignInAgain") }}
        </p>
        <form
          class="grid max-w-sm gap-3"
          aria-labelledby="settings-change-password"
          @submit.prevent="submitPassword"
        >
          <p
            v-if="passwordError"
            class="text-destructive m-0 text-sm"
            role="alert"
          >
            {{ $t(passwordError) }}
          </p>
          <Label for="settings-current-password">{{
            $t("currentPassword")
          }}</Label>
          <Input
            id="settings-current-password"
            v-model="currentPassword"
            type="password"
            name="current-password"
            autocomplete="current-password"
            required
            :disabled="changingPassword"
          />
          <Label for="settings-new-password">{{ $t("newPassword") }}</Label>
          <Input
            id="settings-new-password"
            v-model="newPassword"
            type="password"
            name="new-password"
            autocomplete="new-password"
            required
            :disabled="changingPassword"
          />
          <Label for="settings-confirm-password">{{
            $t("confirmNewPassword")
          }}</Label>
          <Input
            id="settings-confirm-password"
            v-model="confirmNewPassword"
            type="password"
            name="confirm-password"
            autocomplete="new-password"
            required
            :disabled="changingPassword"
          />
          <Button
            class="justify-self-start"
            type="submit"
            :disabled="changingPassword"
          >
            {{ $t(changingPassword ? "changingPassword" : "changePassword") }}
          </Button>
        </form>
      </section>
      <section
        class="bg-card border-border min-w-0 rounded-xl border p-5 shadow-xs"
        aria-labelledby="settings-workspace"
      >
        <header class="border-border border-b pb-3">
          <p
            class="text-muted-foreground m-0 mb-1 text-xs font-medium tracking-wide"
          >
            {{ $t("workspace") }}
          </p>
          <h2 id="settings-workspace" class="m-0 text-sm font-semibold">
            {{ $t("workspaceInformation") }}
          </h2>
        </header>
        <dl class="m-0 mt-2 text-sm">
          <div
            class="border-border flex justify-between gap-4 border-b py-2.5 last:border-b-0"
          >
            <dt class="text-muted-foreground">{{ $t("workspaceName") }}</dt>
            <dd class="m-0 text-right wrap-anywhere">
              {{ workspace?.name ?? $t("notAvailable") }}
            </dd>
          </div>
          <div
            class="border-border flex justify-between gap-4 border-b py-2.5 last:border-b-0"
          >
            <dt class="text-muted-foreground">{{ $t("workspaceSlug") }}</dt>
            <dd class="m-0 text-right wrap-anywhere">
              {{ workspace?.slug ?? $t("notAvailable") }}
            </dd>
          </div>
          <div
            class="border-border flex justify-between gap-4 border-b py-2.5 last:border-b-0"
          >
            <dt class="text-muted-foreground">{{ $t("workspaceId") }}</dt>
            <dd class="m-0 text-right wrap-anywhere">
              <code class="font-mono text-xs">{{
                workspace?.id ?? $t("notAvailable")
              }}</code>
            </dd>
          </div>
        </dl>
      </section>
      <section
        class="bg-card border-border min-w-0 rounded-xl border p-5 shadow-xs"
        aria-labelledby="settings-platform"
      >
        <header class="border-border border-b pb-3">
          <p
            class="text-muted-foreground m-0 mb-1 text-xs font-medium tracking-wide"
          >
            {{ $t("platform") }}
          </p>
          <h2 id="settings-platform" class="m-0 text-sm font-semibold">
            {{ $t("platformContext") }}
          </h2>
        </header>
        <dl class="m-0 mt-2 text-sm">
          <div
            class="border-border flex justify-between gap-4 border-b py-2.5 last:border-b-0"
          >
            <dt class="text-muted-foreground">{{ $t("readiness") }}</dt>
            <dd
              class="m-0 inline-flex items-center gap-1.5 text-right"
              :class="readiness.isReady ? 'text-success' : 'text-destructive'"
            >
              <CheckCircle2
                v-if="readiness.isReady"
                :size="16"
                aria-hidden="true"
              />
              <CircleAlert v-else :size="16" aria-hidden="true" />
              {{ $t(readiness.isReady ? "ready" : "unavailable") }}
            </dd>
          </div>
          <div
            class="border-border flex justify-between gap-4 border-b py-2.5 last:border-b-0"
          >
            <dt class="text-muted-foreground">
              {{ $t("recommendedAgentVersion") }}
            </dt>
            <dd class="m-0 text-right wrap-anywhere">
              {{
                buildUnavailable
                  ? $t("recommendationUnavailable")
                  : buildInfo?.recommendedAgentVersion ||
                    $t("recommendationNotConfigured")
              }}
            </dd>
          </div>
        </dl>
      </section>
    </div>
  </main>
</template>
