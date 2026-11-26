<script setup lang="ts">
import { ResponseError } from "@/api/transport";
import type { Workspace } from "@/api/models";
import { defaultWindow, useEventListener } from "@vueuse/core";
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { createNodeBootstrapToken } from "../../api/enrollment";
import { getWorkspace } from "../../api/workspace";
import {
  installCommand,
  validInstallConfig,
  validInstallField,
  type NodeInstallConfig,
} from "../../features/nodes/node-install";
import OperationDialog from "../common/OperationDialog.vue";
import FormField from "../common/FormField.vue";
import { Button } from "../ui/button";
import { Input } from "../ui/input";
import { Textarea } from "../ui/textarea";

const emit = defineEmits<{ close: [] }>();
const config = ref<NodeInstallConfig>({
  name: "",
  environment: "production",
  release: "",
});
const fields: (keyof NodeInstallConfig)[] = ["name", "environment", "release"];
const step = ref(1);
const workspace = ref<Workspace>();
const token = ref("");
const expires = ref("");
const error = ref("");
const attempted = ref(false);
const pending = ref(false);
let expiryTimer: ReturnType<typeof setTimeout> | undefined;
const controller = new AbortController();
const isActive = () => !controller.signal.aborted;
const command = computed(() => installCommand(config.value.release));

function clear(): void {
  controller.abort();
  clearTimeout(expiryTimer);
  token.value = "";
  expires.value = "";
}
function close(): void {
  clear();
  emit("close");
}
onBeforeUnmount(clear);
// Full-document navigation can retain the page in the browser back/forward cache.
useEventListener(defaultWindow, "pagehide", close);
onMounted(async () => {
  try {
    const selected = await getWorkspace();
    if (isActive()) workspace.value = selected;
  } catch {
    if (isActive()) error.value = "installWorkspaceFailed";
  }
});

function prepare(): void {
  if (step.value === 1 && workspace.value && validInstallConfig(config.value))
    step.value = 2;
}
async function issue(): Promise<void> {
  if (
    !isActive() ||
    step.value !== 2 ||
    attempted.value ||
    !workspace.value ||
    !validInstallConfig(config.value)
  )
    return;
  attempted.value = true;
  pending.value = true;
  error.value = "";
  try {
    const result = await createNodeBootstrapToken(
      workspace.value.id,
      config.value.environment,
      config.value.name,
      controller.signal,
    );
    if (!isActive()) return;
    const expiry = Date.parse(result.expires_at);
    if (
      !/^oit1_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]{43}$/.test(result.token) ||
      !Number.isFinite(expiry)
    ) {
      error.value = "installUnknown";
      return;
    }
    token.value = result.token;
    expires.value = result.expires_at;
    const remaining = expiry - Date.now();
    const delay = remaining > 0 ? Math.min(remaining, 900_000) : 900_000;
    expiryTimer = setTimeout(() => {
      token.value = "";
      error.value = "installExpired";
    }, delay);
  } catch (cause) {
    if (!isActive()) return;
    error.value =
      cause instanceof ResponseError && cause.response.status === 403
        ? "installForbidden"
        : cause instanceof ResponseError && cause.response.status === 400
          ? "installBadRequest"
          : "installUnknown";
  } finally {
    if (isActive()) pending.value = false;
  }
}
</script>

<template>
  <OperationDialog
    :title="$t('installNode')"
    :error="error ? $t(error) : ''"
    wide
    @close="close"
    @submit="prepare"
  >
    <template #description>{{ $t("installIntro") }}</template>
    <p class="m-0 text-sm font-medium">{{ $t(`installStep${step}`) }}</p>
    <template v-if="step === 1">
      <FormField id="install-workspace" :label="$t('workspace')">
        <output id="install-workspace" class="text-sm break-all"
          >{{ workspace?.name }} · {{ workspace?.id }}</output
        >
      </FormField>
      <FormField
        v-for="field in fields"
        :id="`install-${field}`"
        :key="field"
        :label="$t(`installField_${field}`)"
        :help="$t(`installHint_${field}`)"
      >
        <Input
          :id="`install-${field}`"
          v-model="config[field]"
          :aria-describedby="`install-${field}-help`"
          autocomplete="off"
          required
          :aria-invalid="
            config[field] !== '' && !validInstallField(field, config[field])
          "
        />
      </FormField>
    </template>
    <template v-else>
      <FormField id="install-command" :label="$t('installRun')">
        <Textarea
          id="install-command"
          :model-value="command"
          readonly
          rows="5"
          class="font-mono text-xs"
        />
      </FormField>
      <p class="m-0 text-sm font-medium">
        {{ $t("installPreparation") }}
      </p>
      <p class="m-0 text-sm">{{ $t("installTokenNotice") }}</p>
      <Button type="button" :disabled="attempted" @click="issue">{{
        $t(pending ? "installIssuing" : "installIssue")
      }}</Button>
      <template v-if="token">
        <FormField id="install-token" :label="$t('installToken')">
          <Textarea
            id="install-token"
            :model-value="token"
            readonly
            autocomplete="off"
            spellcheck="false"
            class="font-mono text-xs"
          />
        </FormField>
        <p class="m-0 text-sm">
          {{ $t("installExpires", { time: expires }) }}
        </p>
      </template>
    </template>
    <template #footer>
      <Button
        v-if="step === 1"
        type="submit"
        :disabled="!workspace || !validInstallConfig(config)"
        >{{ $t("installPrepare") }}</Button
      >
      <Button v-else type="button" @click="close">{{
        $t("closeDialog")
      }}</Button>
    </template>
  </OperationDialog>
</template>
