<script setup lang="ts">
import { expiryInputType, type UserPolicyForm } from "../adapters/user-policy";
import FormField from "@/components/common/FormField.vue";
import { Input } from "@/components/ui/input";
import { NativeSelect } from "@/components/ui/native-select";

const props = defineProps<{ modelValue: UserPolicyForm }>();
const emit = defineEmits<{ "update:modelValue": [value: UserPolicyForm] }>();

function update<K extends keyof UserPolicyForm>(
  key: K,
  value: UserPolicyForm[K],
): void {
  emit("update:modelValue", { ...props.modelValue, [key]: value });
}
</script>

<template>
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
        :model-value="modelValue.period"
        @update:model-value="
          update('period', $event as UserPolicyForm['period'])
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
        :model-value="modelValue.direction"
        :disabled="modelValue.period === 'none'"
        @update:model-value="
          update('direction', $event as UserPolicyForm['direction'])
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
          :disabled="modelValue.period === 'none'"
          :model-value="modelValue.quotaValue"
          @update:model-value="update('quotaValue', Number($event))"
        />
        <NativeSelect
          class="w-24"
          :model-value="modelValue.quotaUnit"
          :disabled="modelValue.period === 'none'"
          :aria-label="$t('quotaUnit')"
          @update:model-value="
            update('quotaUnit', $event as UserPolicyForm['quotaUnit'])
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
        :type="expiryInputType(modelValue.expiresAtLocal)"
        step="1"
        :aria-describedby="describedBy"
        :model-value="modelValue.expiresAtLocal"
        @update:model-value="update('expiresAtLocal', String($event))"
      />
    </FormField>
  </fieldset>
</template>
