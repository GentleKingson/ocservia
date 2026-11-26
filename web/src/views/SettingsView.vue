<script setup lang="ts">
import { CheckCircle2, CircleAlert } from "@lucide/vue";
import type { BuildInfo, Workspace } from "@/api/models";
import { ResponseError } from "@/api/transport";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { onBeforeUnmount, onMounted, ref } from "vue";

import DataState from "../components/common/DataState.vue";
import FormField from "../components/common/FormField.vue";
import PageHeader from "../components/common/PageHeader.vue";
import SectionCard from "../components/common/SectionCard.vue";
import { getWorkspace } from "../api/workspace";
import { changeLocalPassword, getVersion } from "../api/platform";
import { createReadRequest } from "../shared/read-request";
import { useReadinessStore } from "../shared/readiness";

const readiness = useReadinessStore();
const workspace = ref<Workspace>();
const buildInfo = ref<BuildInfo>();
const loading = ref(true);
const unavailable = ref(false);
const buildUnavailable = ref(false);
const reads = createReadRequest();
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
  const request = reads.begin();
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
    if (!request.current()) return;
    workspace.value = workspaceResult;
    buildInfo.value = buildResult.value;
    buildUnavailable.value = buildResult.unavailable;
  } catch {
    if (!request.current()) return;
    workspace.value = undefined;
    unavailable.value = true;
  } finally {
    if (request.finish()) loading.value = false;
  }
}

onMounted(() => {
  void loadWorkspace();
});
onBeforeUnmount(reads.cancel);
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
      <SectionCard class="mb-0 md:col-span-2" :title="$t('changePassword')">
        <p class="text-muted-foreground m-0 text-sm">
          {{ $t("passwordChangeSignInAgain") }}
        </p>
        <form
          class="grid max-w-sm gap-3"
          :aria-label="$t('changePassword')"
          @submit.prevent="submitPassword"
        >
          <p
            v-if="passwordError"
            class="text-destructive m-0 text-sm"
            role="alert"
          >
            {{ $t(passwordError) }}
          </p>
          <FormField
            id="settings-current-password"
            :label="$t('currentPassword')"
          >
            <Input
              id="settings-current-password"
              v-model="currentPassword"
              type="password"
              name="current-password"
              autocomplete="current-password"
              required
              :disabled="changingPassword"
            />
          </FormField>
          <FormField id="settings-new-password" :label="$t('newPassword')">
            <Input
              id="settings-new-password"
              v-model="newPassword"
              type="password"
              name="new-password"
              autocomplete="new-password"
              required
              :disabled="changingPassword"
            />
          </FormField>
          <FormField
            id="settings-confirm-password"
            :label="$t('confirmNewPassword')"
          >
            <Input
              id="settings-confirm-password"
              v-model="confirmNewPassword"
              type="password"
              name="confirm-password"
              autocomplete="new-password"
              required
              :disabled="changingPassword"
            />
          </FormField>
          <Button
            class="justify-self-start"
            type="submit"
            :disabled="changingPassword"
          >
            {{ $t(changingPassword ? "changingPassword" : "changePassword") }}
          </Button>
        </form>
      </SectionCard>
      <SectionCard class="mb-0" :title="$t('workspaceInformation')">
        <dl class="m-0 text-sm">
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
      </SectionCard>
      <SectionCard class="mb-0" :title="$t('platformContext')">
        <dl class="m-0 text-sm">
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
      </SectionCard>
    </div>
  </main>
</template>
