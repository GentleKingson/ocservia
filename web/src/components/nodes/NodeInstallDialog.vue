<script setup lang="ts">
import { ResponseError, type Workspace } from "@ocservia/api-client";
import { defaultWindow, useEventListener } from "@vueuse/core";
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { createNodeBootstrapToken } from "../../api/enrollment";
import {
  getWorkspace,
  sameWorkspaceContext,
  workspaceChangedEvent,
  workspaceContext,
} from "../../api/workspace";
import {
  installCommand,
  installEnv,
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
  controller: "",
  relay: "",
  bootstrapFile: "/protected/node-bootstrap-token",
  relayFile: "/protected/relay-access-token",
  commandKeyFile: "/protected/controller-command-public.pem",
});
const fields: (keyof NodeInstallConfig)[] = [
  "name",
  "environment",
  "release",
  "controller",
  "relay",
  "bootstrapFile",
  "relayFile",
  "commandKeyFile",
];
const step = ref(1);
const workspace = ref<Workspace>();
const token = ref("");
const expires = ref("");
const error = ref("");
const attempted = ref(false);
const pending = ref(false);
const copied = ref(false);
let active = true;
let expiryTimer: ReturnType<typeof setTimeout> | undefined;
const controller = new AbortController();
const template = computed(() =>
  installEnv(config.value, workspace.value?.id ?? ""),
);
const command = computed(() => installCommand(config.value.release));

function clear(): void {
  active = false;
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
useEventListener(defaultWindow, workspaceChangedEvent, close);
// Full-document navigation can retain the page in the browser back/forward cache.
useEventListener(defaultWindow, "pagehide", close);
onMounted(async () => {
  try {
    const selected = await getWorkspace();
    if (active) workspace.value = selected;
  } catch {
    if (active) error.value = "installWorkspaceFailed";
  }
});

function prepare(): void {
  if (
    step.value === 1 &&
    workspace.value &&
    validInstallConfig(config.value) &&
    template.value
  )
    step.value = 2;
}
async function copyConfig(): Promise<void> {
  try {
    await navigator.clipboard.writeText(template.value);
    if (active) {
      copied.value = true;
      if (error.value === "installCopyFailed") error.value = "";
    }
  } catch {
    if (active) error.value = "installCopyFailed";
  }
}
async function issue(): Promise<void> {
  if (
    !active ||
    step.value !== 2 ||
    attempted.value ||
    !workspace.value ||
    !template.value
  )
    return;
  const context = workspaceContext();
  if (context.id !== workspace.value.id) {
    close();
    return;
  }
  const owned = () =>
    active && sameWorkspaceContext(context, workspaceContext());
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
    if (!owned()) return;
    const expiry = result.expiresAt.getTime();
    if (
      !/^obt1_[A-Za-z0-9_-]{43}$/.test(result.token) ||
      !Number.isFinite(expiry) ||
      expiry <= Date.now()
    ) {
      error.value = "installUnknown";
      return;
    }
    token.value = result.token;
    expires.value = result.expiresAt.toISOString();
    expiryTimer = setTimeout(
      () => {
        token.value = "";
        error.value = "installExpired";
      },
      Math.min(expiry - Date.now(), 900_000),
    );
  } catch (cause) {
    if (!owned()) return;
    error.value =
      cause instanceof ResponseError && cause.response.status === 403
        ? "installForbidden"
        : cause instanceof ResponseError && cause.response.status === 400
          ? "installBadRequest"
          : "installUnknown";
  } finally {
    if (owned()) pending.value = false;
  }
}
function next(): void {
  // The token is displayed only in step 2 and cannot be reopened.
  token.value = "";
  clearTimeout(expiryTimer);
  error.value = "";
  step.value = 3;
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
      <p class="text-muted-foreground m-0 text-sm">{{ $t("installPaths") }}</p>
    </template>
    <template v-else>
      <FormField id="install-template" :label="$t('installTemplate')">
        <Textarea
          id="install-template"
          :model-value="template"
          readonly
          rows="10"
          class="font-mono text-xs"
        />
      </FormField>
      <Button type="button" variant="outline" @click="copyConfig">{{
        $t(copied ? "installCopied" : "installCopy")
      }}</Button>
      <p class="m-0 text-sm">{{ $t("installSaveConfig") }}</p>
      <p class="m-0 text-sm">{{ $t("installFiles") }}</p>
      <FormField id="install-command" :label="$t('installRun')">
        <Textarea
          id="install-command"
          :model-value="command"
          readonly
          rows="5"
          class="font-mono text-xs"
        />
      </FormField>
      <p v-if="step === 2" class="m-0 text-sm">
        {{ $t("installPreparation") }}
      </p>
      <template v-if="step === 2">
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
      <template v-else>
        <p class="m-0 text-sm">{{ $t("installLocal") }}</p>
        <p class="m-0 text-sm">{{ $t("installApproval") }}</p>
        <a
          class="text-primary underline"
          href="https://github.com/GentleKingson/ocservia/blob/main/docs/operations/agent-lifecycle.md#approve-the-node"
          target="_blank"
          rel="noopener noreferrer"
          >{{ $t("installApprovalGuide") }}</a
        >
        <pre class="bg-muted m-0 overflow-x-auto rounded-md p-3 text-xs">
sudo systemctl enable --now \
  ocservia-privd.service \
  ocservia-agent.service</pre>
        <p class="m-0 text-sm">{{ $t("installVerify") }}</p>
        <p class="m-0 text-sm">{{ $t("installRecovery") }}</p>
      </template>
    </template>
    <template #footer>
      <Button v-if="step === 1" type="submit" :disabled="!template">{{
        $t("installPrepare")
      }}</Button>
      <Button
        v-else-if="step === 2"
        type="button"
        :disabled="!token"
        @click="next"
        >{{ $t("installContinue") }}</Button
      >
      <Button v-else type="button" @click="close">{{
        $t("closeDialog")
      }}</Button>
    </template>
  </OperationDialog>
</template>
