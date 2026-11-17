<script setup lang="ts">
import { onBeforeUnmount, ref } from "vue";
import { useI18n } from "vue-i18n";
import { workspaceContext } from "../../api/workspace";
import {
  expiryInputType,
  loadUserPolicy,
  policyToForm,
  saveUserPolicy,
  type UserPolicyForm,
} from "../../features/user-policy";
import { createNodeWorkflow } from "../../features/node-workflow";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { NativeSelect } from "@/components/ui/native-select";
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

function updatePolicyField<K extends keyof UserPolicyForm>(
  key: K,
  value: UserPolicyForm[K],
): void {
  policyForm.value = { ...policyForm.value, [key]: value };
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
    <fieldset class="m-0 grid min-w-0 gap-4 border-0 p-0">
      <legend class="mb-4 p-0 text-sm font-semibold">
        {{ $t("quotaAndExpiry") }}
      </legend>
      <FormField
        id="quota-period"
        :label="$t('quotaPeriod')"
        :help="$t('quotaPeriodHelp')"
        v-slot="{ describedBy }"
      >
        <NativeSelect
          id="quota-period"
          :aria-describedby="describedBy"
          :model-value="policyForm.period"
          @update:model-value="
            updatePolicyField('period', $event as UserPolicyForm['period'])
          "
        >
          <option value="none">{{ $t("quotaNone") }}</option>
          <option value="monthly">{{ $t("quotaMonthly") }}</option>
          <option value="lifetime">{{ $t("quotaLifetime") }}</option>
        </NativeSelect>
      </FormField>
      <FormField id="quota-direction" :label="$t('quotaDirection')">
        <NativeSelect
          id="quota-direction"
          :model-value="policyForm.direction"
          :disabled="policyForm.period === 'none'"
          @update:model-value="
            updatePolicyField(
              'direction',
              $event as UserPolicyForm['direction'],
            )
          "
        >
          <option value="rx">{{ $t("quotaReceive") }}</option>
          <option value="tx">{{ $t("quotaTransmit") }}</option>
          <option value="rxtx">{{ $t("quotaCombined") }}</option>
        </NativeSelect>
      </FormField>
      <FormField
        id="quota-value"
        :label="$t('quotaSize')"
        :help="$t('quotaSizeHelp')"
        v-slot="{ describedBy }"
      >
        <div class="flex gap-2">
          <Input
            id="quota-value"
            class="max-w-48"
            type="number"
            min="0"
            step="0.01"
            :aria-describedby="describedBy"
            :disabled="policyForm.period === 'none'"
            :model-value="policyForm.quotaValue"
            @update:model-value="
              updatePolicyField('quotaValue', Number($event))
            "
          />
          <NativeSelect
            class="w-24"
            :model-value="policyForm.quotaUnit"
            :disabled="policyForm.period === 'none'"
            :aria-label="$t('quotaUnit')"
            @update:model-value="
              updatePolicyField(
                'quotaUnit',
                $event as UserPolicyForm['quotaUnit'],
              )
            "
          >
            <option value="MiB">MiB</option>
            <option value="GiB">GiB</option>
          </NativeSelect>
        </div>
      </FormField>
      <FormField
        id="expires-at"
        :label="$t('expiresAtUtc')"
        :help="$t('expiresAtHelp')"
        v-slot="{ describedBy }"
      >
        <Input
          id="expires-at"
          :type="expiryInputType(policyForm.expiresAtUtcInput)"
          step="1"
          :aria-describedby="describedBy"
          :model-value="policyForm.expiresAtUtcInput"
          @update:model-value="
            updatePolicyField('expiresAtUtcInput', String($event))
          "
        />
      </FormField>
    </fieldset>
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
