<script setup lang="ts">
import { onBeforeUnmount, ref } from "vue";
import { useI18n } from "vue-i18n";
import { workspaceContext } from "../../api/workspace";
import {
  loadUserPolicy,
  policyToForm,
  saveUserPolicy,
  type UserPolicyForm,
} from "../../adapters/user-policy";
import { createNodeWorkflow } from "../../features/node-workflow";
import UserPolicyFields from "../../upstream/UserPolicyFields.vue";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import FormField from "../common/FormField.vue";
import OperationDialog from "../common/OperationDialog.vue";

// Mount with v-if and key by username: mounting reads the policy, and every
// close path (including the parent's node/workspace change) unmounts and
// detaches late responses. A save already sent is not undone by closing.
const props = defineProps<{
  nodeId: string | undefined;
  username: string;
}>();
const emit = defineEmits<{ close: [] }>();
const { t } = useI18n();

const policyForm = ref<UserPolicyForm>(policyToForm());
const policyReason = ref("");
const policyLoading = ref(false);
const policyError = ref("");
let open = true;
const policyWorkflow = createNodeWorkflow(
  () => props.nodeId,
  () => open,
  workspaceContext,
);
const policyContext = props.nodeId
  ? policyWorkflow.begin(props.nodeId)
  : undefined;
onBeforeUnmount(() => {
  open = false;
  policyWorkflow.cancel();
});

async function loadPolicy(): Promise<void> {
  const context = policyContext;
  if (!context) return;
  policyLoading.value = true;
  try {
    const loaded = await loadUserPolicy(
      context.nodeId,
      props.username,
      context.signal,
    );
    if (policyWorkflow.isCurrent(context)) policyForm.value = loaded;
  } catch (error) {
    if (!policyWorkflow.isCurrent(context)) return;
    policyError.value =
      error instanceof Error ? error.message : t("policyLoadFailed");
  } finally {
    if (policyWorkflow.isCurrent(context)) policyLoading.value = false;
  }
}
void loadPolicy();

async function submitPolicy(): Promise<void> {
  const context = policyContext;
  if (
    !context ||
    !policyWorkflow.isCurrent(context) ||
    policyLoading.value ||
    !policyReason.value.trim()
  )
    return;
  policyLoading.value = true;
  policyError.value = "";
  try {
    const saved = await saveUserPolicy(
      context.nodeId,
      props.username,
      policyForm.value,
      policyReason.value.trim(),
    );
    if (!policyWorkflow.isCurrent(context)) return;
    policyForm.value = saved;
    emit("close");
  } catch (error) {
    if (!policyWorkflow.isCurrent(context)) return;
    policyError.value =
      error instanceof Error ? error.message : t("policyUpdateFailed");
  } finally {
    if (policyWorkflow.isCurrent(context)) policyLoading.value = false;
  }
}
</script>

<template>
  <OperationDialog
    :title="$t('quotaAndExpiry')"
    :subject="username"
    :error="policyError"
    @close="emit('close')"
    @submit="submitPolicy"
  >
    <UserPolicyFields v-model="policyForm" />
    <FormField id="policy-reason" :label="$t('reason')">
      <Textarea
        id="policy-reason"
        v-model="policyReason"
        maxlength="512"
        required
      />
    </FormField>
    <template #footer>
      <Button type="submit" :disabled="policyLoading || !policyReason.trim()">
        {{ $t("confirm") }}
      </Button>
    </template>
  </OperationDialog>
</template>
