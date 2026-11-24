<script setup lang="ts">
import type { Ref } from "vue";
import { ResponseError } from "@/api/transport";
import type { NodeObservedState } from "@/api/models";
import type { DesiredDialogKind } from "@/shared/desired-recovery";
import { ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { getUserPasswordSealingKey } from "@/api/users";
import { sealUserPassword } from "@/features/user-password";
import { cachedWorkspaceId } from "@/api/workspace";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import FormField from "@/components/common/FormField.vue";
import OperationDialog from "@/components/common/OperationDialog.vue";
import { useFleetStore } from "@/shared/fleet";
import { createNodeWorkflow } from "@/features/node-workflow";

const props = defineProps<{
  context: { currentNode: Readonly<Ref<NodeObservedState | undefined>> };
}>();
const { currentNode } = props.context;
const fleet = useFleetStore();
const { t } = useI18n();
const readReady = () =>
  fleet.userGroupReadState === "ready" && !fleet.selectionError;
const desiredDialog = ref<{
  kind: DesiredDialogKind;
  name: string;
  version: number;
}>();
const desiredName = ref("");
const desiredPassword = ref("");
const desiredLoading = ref(false);
const desiredError = ref("");
const groupMembers = ref("");
const desiredReason = ref("");
function openDesired(kind: DesiredDialogKind, name = "", version = 0): void {
  desiredDialog.value = { kind, name, version };
  desiredName.value = name;
  desiredWorkflow.cancel();
  desiredPassword.value = "";
  desiredError.value = "";
  desiredLoading.value = false;
  groupMembers.value =
    kind === "group"
      ? (
          fleet.userGroupState.find(
            (item) => item.kind === "group" && item.name === name,
          )?.desiredMembers ?? []
        ).join(", ")
      : "";
  desiredReason.value = "";
}

const desiredWorkflow = createNodeWorkflow(
  () => currentNode.value?.id,
  () => Boolean(desiredDialog.value),
  cachedWorkspaceId,
);
watch(
  desiredDialog,
  (dialog) => {
    if (dialog) return;
    desiredWorkflow.cancel();
    desiredPassword.value = "";
    desiredError.value = "";
    desiredLoading.value = false;
  },
  { flush: "sync" },
);

async function submitDesired(): Promise<void> {
  const dialog = desiredDialog.value;
  const explanation = desiredReason.value.trim();
  const nodeId = currentNode.value?.id;
  if (
    !dialog ||
    !explanation ||
    !nodeId ||
    desiredLoading.value ||
    !readReady()
  )
    return;
  const name = desiredName.value.trim();
  const context = desiredWorkflow.begin(nodeId);
  desiredLoading.value = true;
  desiredError.value = "";
  // UTF-8 bytes are transient and never passed to the API or fleet store.
  const plaintext = new TextEncoder().encode(desiredPassword.value);
  desiredPassword.value = "";
  try {
    if (dialog.kind === "create" || dialog.kind === "rotate") {
      const key = await getUserPasswordSealingKey(nodeId, context.signal);
      if (!desiredWorkflow.isCurrent(context)) return;
      const sealed = await sealUserPassword(
        plaintext,
        key,
        context.workspaceId,
        nodeId,
      );
      if (!desiredWorkflow.isCurrent(context)) return;
      if (!readReady()) {
        desiredError.value = t("nodeSectionStale");
        return;
      }
      desiredDialog.value = undefined;
      if (dialog.kind === "create")
        await fleet.createUser(
          name,
          dialog.version,
          sealed.ciphertext,
          sealed.keyId,
          explanation,
        );
      else
        await fleet.rotateUserPassword(
          dialog.name,
          dialog.version,
          sealed.ciphertext,
          sealed.keyId,
          explanation,
        );
    } else {
      desiredDialog.value = undefined;
      if (dialog.kind === "disable")
        await fleet.disableUser(dialog.name, dialog.version, explanation);
      else if (dialog.kind === "enable")
        await fleet.enableUser(dialog.name, dialog.version, explanation);
      else
        await fleet.applyGroup(
          name,
          dialog.version,
          groupMembers.value
            .split(",")
            .map((value) => value.trim())
            .filter(Boolean)
            .sort(),
          explanation,
        );
    }
  } catch (error) {
    if (!desiredWorkflow.isCurrent(context)) return;
    const keys = [
      "passwordCryptoUnavailable",
      "passwordKeyChanged",
      "passwordTooLong",
    ];
    desiredError.value = t(
      error instanceof Error && keys.includes(error.message)
        ? error.message
        : error instanceof ResponseError && error.response.status === 403
          ? "passwordKeyForbidden"
          : error instanceof ResponseError && error.response.status === 409
            ? "passwordKeyChanged"
            : "passwordKeyUnavailable",
    );
  } finally {
    plaintext.fill(0);
    if (desiredWorkflow.isCurrent(context)) desiredLoading.value = false;
  }
}

function close() {
  desiredDialog.value = undefined;
}
defineExpose({ open: openDesired, close });
</script>
<template>
  <OperationDialog
    v-if="desiredDialog"
    :title="
      $t(
        desiredDialog.kind === 'create'
          ? 'createUser'
          : desiredDialog.kind === 'disable'
            ? 'disableUser'
            : desiredDialog.kind === 'enable'
              ? 'enableUser'
              : desiredDialog.kind === 'rotate'
                ? 'rotatePassword'
                : 'applyGroup',
      )
    "
    :subject="desiredDialog.name || currentNode?.name"
    :error="desiredError"
    @close="desiredDialog = undefined"
    @submit="submitDesired"
  >
    <FormField
      v-if="desiredDialog.kind === 'create' || desiredDialog.kind === 'group'"
      id="desired-name"
      :label="$t(desiredDialog.kind === 'group' ? 'group' : 'user')"
    >
      <Input
        id="desired-name"
        v-model="desiredName"
        :disabled="desiredDialog.kind === 'create' && desiredDialog.version > 0"
        maxlength="64"
        required
      />
    </FormField>
    <FormField
      v-if="desiredDialog.kind === 'create' || desiredDialog.kind === 'rotate'"
      id="desired-password"
      :label="$t('password')"
      :help="$t('passwordSealingHelp')"
      v-slot="{ describedBy }"
    >
      <Input
        id="desired-password"
        v-model="desiredPassword"
        type="password"
        autocomplete="new-password"
        :aria-describedby="describedBy"
        :disabled="desiredLoading"
        required
      />
    </FormField>
    <FormField
      v-if="desiredDialog.kind === 'group'"
      id="group-members"
      :label="$t('members')"
    >
      <Textarea id="group-members" v-model="groupMembers" maxlength="65535" />
    </FormField>
    <FormField id="desired-reason" :label="$t('reason')">
      <Textarea
        id="desired-reason"
        v-model="desiredReason"
        maxlength="512"
        required
      />
    </FormField>
    <template #footer>
      <Button
        type="submit"
        :variant="desiredDialog.kind === 'disable' ? 'destructive' : 'default'"
        :disabled="
          desiredLoading ||
          !readReady() ||
          !desiredReason.trim() ||
          ((desiredDialog.kind === 'create' ||
            desiredDialog.kind === 'rotate') &&
            !desiredPassword)
        "
      >
        {{ $t("confirm") }}
      </Button>
    </template>
  </OperationDialog>
</template>
