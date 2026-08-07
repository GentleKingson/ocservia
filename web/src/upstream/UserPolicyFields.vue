<script setup lang="ts">
import type { UserPolicyForm } from "../adapters/user-policy";

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
  <fieldset class="policy-fields">
    <legend>{{ $t("quotaAndExpiry") }}</legend>
    <label for="quota-period">{{ $t("quotaPeriod") }}</label>
    <select
      id="quota-period"
      :value="modelValue.period"
      @change="
        update(
          'period',
          ($event.target as HTMLSelectElement)
            .value as UserPolicyForm['period'],
        )
      "
    >
      <option value="none">{{ $t("quotaNone") }}</option>
      <option value="monthly">{{ $t("quotaMonthly") }}</option>
      <option value="lifetime">{{ $t("quotaLifetime") }}</option>
    </select>
    <label for="quota-direction">{{ $t("quotaDirection") }}</label>
    <select
      id="quota-direction"
      :value="modelValue.direction"
      :disabled="modelValue.period === 'none'"
      @change="
        update(
          'direction',
          ($event.target as HTMLSelectElement)
            .value as UserPolicyForm['direction'],
        )
      "
    >
      <option value="rx">{{ $t("quotaReceive") }}</option>
      <option value="tx">{{ $t("quotaTransmit") }}</option>
      <option value="rxtx">{{ $t("quotaCombined") }}</option>
    </select>
    <label for="quota-value">{{ $t("quotaSize") }}</label>
    <div class="policy-size">
      <input
        id="quota-value"
        type="number"
        min="0"
        step="0.01"
        :disabled="modelValue.period === 'none'"
        :value="modelValue.quotaValue"
        @input="
          update(
            'quotaValue',
            Number(($event.target as HTMLInputElement).value),
          )
        "
      />
      <select
        :value="modelValue.quotaUnit"
        :disabled="modelValue.period === 'none'"
        :aria-label="$t('quotaUnit')"
        @change="
          update(
            'quotaUnit',
            ($event.target as HTMLSelectElement)
              .value as UserPolicyForm['quotaUnit'],
          )
        "
      >
        <option value="MiB">MiB</option>
        <option value="GiB">GiB</option>
      </select>
    </div>
    <label for="expires-at">{{ $t("expiresAtUtc") }}</label>
    <input
      id="expires-at"
      type="datetime-local"
      :value="modelValue.expiresAtLocal"
      @input="
        update('expiresAtLocal', ($event.target as HTMLInputElement).value)
      "
    />
  </fieldset>
</template>
