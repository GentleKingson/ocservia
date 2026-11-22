<script setup lang="ts">
import type { Ref } from "vue";
import type { NodeObservedState } from "@ocservia/api-client";
import { useI18n } from "vue-i18n";
import { workspaceContext } from "@/api/workspace";
import { useNodeConfiguration } from "@/features/configuration/useNodeConfiguration";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import FormField from "@/components/common/FormField.vue";
import OperationDialog from "@/components/common/OperationDialog.vue";
import { useFleetStore } from "@/shared/fleet";
import { operationStatusKey } from "@/shared/operation-status";

const props = defineProps<{
  context: {
    currentNode: Readonly<Ref<NodeObservedState | undefined>>;
    readReady: Readonly<Ref<boolean>>;
  };
  actionAvailable: (action: string) => boolean;
  actionExplanation: (action: string) => string;
}>();
const { currentNode } = props.context;
const fleet = useFleetStore();
const { t } = useI18n();
const {
  configDialog,
  currentConfigRevision,
  configSourceRevision,
  canSubmitConfigPlan,
  configPlan,
  configOperation,
  configError,
  configLoading,
  configPort,
  configUdpPort,
  configDevice,
  configNetwork,
  configDns,
  configCookieTimeout,
  configMaxSameClients,
  configMaxClients,
  configRoute,
  configCertificateSecretRefId,
  configPrivateKeySecretRefId,
  configReason,
  configApplyReason,
  openConfigPlan,
  submitConfigPlan,
  submitConfigApply,
} = useNodeConfiguration({
  currentNode,
  readReady: props.context.readReady,
  workspaceContext,
  trackOperation: (id) => fleet.trackOperation(id),
  t,
});

function close() {
  configDialog.value = false;
}
defineExpose({ open: openConfigPlan, close, currentConfigRevision });
</script>
<template>
  <OperationDialog
    v-if="configDialog"
    wide
    :title="$t('configPlan')"
    :subject="currentNode?.name"
    :error="configError"
    @close="configDialog = false"
    @submit="submitConfigPlan"
  >
    <template #description>
      <span class="mt-2 block" role="note">{{
        $t("configTemplateSource")
      }}</span>
    </template>
    <div class="text-muted-foreground grid gap-1 text-xs">
      <p class="m-0">
        {{ $t("configSourceRevision") }}:
        <code>{{ configSourceRevision }}</code>
      </p>
      <p class="m-0">{{ $t("configRiskFields") }}</p>
    </div>
    <div class="grid gap-4 sm:grid-cols-2">
      <FormField id="config-port" :label="$t('tcpPort')">
        <Input
          id="config-port"
          v-model.number="configPort"
          type="number"
          min="1"
          max="65535"
          required
        />
      </FormField>
      <FormField id="config-clients" :label="$t('maxClients')">
        <Input
          id="config-clients"
          v-model.number="configMaxClients"
          type="number"
          min="1"
          max="65535"
          required
        />
      </FormField>
      <FormField id="config-udp-port" :label="$t('udpPort')">
        <Input
          id="config-udp-port"
          v-model.number="configUdpPort"
          type="number"
          min="0"
          max="65535"
          required
        />
      </FormField>
      <FormField id="config-device" :label="$t('vpnDevice')">
        <Input
          id="config-device"
          v-model="configDevice"
          maxlength="15"
          required
        />
      </FormField>
      <FormField id="config-network" :label="$t('ipv4Network')">
        <Input
          id="config-network"
          v-model="configNetwork"
          maxlength="18"
          required
        />
      </FormField>
      <FormField id="config-dns" :label="$t('dnsServer')">
        <Input id="config-dns" v-model="configDns" maxlength="15" required />
      </FormField>
      <FormField id="config-same-clients" :label="$t('maxSameClients')">
        <Input
          id="config-same-clients"
          v-model.number="configMaxSameClients"
          type="number"
          min="1"
          :max="configMaxClients"
          required
        />
      </FormField>
      <FormField id="config-cookie-timeout" :label="$t('cookieTimeout')">
        <Input
          id="config-cookie-timeout"
          v-model.number="configCookieTimeout"
          type="number"
          min="60"
          max="86400"
          required
        />
      </FormField>
      <FormField id="config-route" :label="$t('route')">
        <Input
          id="config-route"
          v-model="configRoute"
          maxlength="256"
          required
        />
      </FormField>
      <FormField id="config-certificate-key" :label="$t('certificateRef')">
        <Input
          id="config-certificate-key"
          v-model="configCertificateSecretRefId"
          maxlength="36"
          required
        />
      </FormField>
      <FormField id="config-private-key" :label="$t('privateKeyRef')">
        <Input
          id="config-private-key"
          v-model="configPrivateKeySecretRefId"
          maxlength="36"
          required
        />
      </FormField>
    </div>
    <FormField id="config-reason" :label="$t('reason')">
      <Textarea
        id="config-reason"
        v-model="configReason"
        maxlength="512"
        required
      />
    </FormField>
    <div
      v-if="configPlan"
      class="bg-muted/50 border-border grid min-w-0 gap-3 rounded-md border p-3"
      data-testid="config-plan-result"
      aria-live="polite"
    >
      <Badge
        variant="outline"
        :class="
          configPlan.validation === 'valid'
            ? 'text-success'
            : 'text-destructive'
        "
        >{{ configPlan.validation }}</Badge
      >
      <dl class="m-0 grid gap-2 text-sm">
        <div class="grid min-w-0 gap-1">
          <dt class="text-muted-foreground text-xs">
            {{ $t("configPlanId") }}
          </dt>
          <dd class="m-0">
            <code class="text-xs break-all">{{ configPlan.id }}</code>
          </dd>
        </div>
        <div class="grid min-w-0 gap-1">
          <dt class="text-muted-foreground text-xs">
            {{ $t("candidateHash") }}
          </dt>
          <dd class="m-0">
            <code class="text-xs break-all">{{
              configPlan.candidateHash
            }}</code>
          </dd>
        </div>
        <div v-if="configPlan.materializedHash" class="grid min-w-0 gap-1">
          <dt class="text-muted-foreground text-xs">
            {{ $t("materializedHash") }}
          </dt>
          <dd class="m-0">
            <code class="text-xs break-all">{{
              configPlan.materializedHash
            }}</code>
          </dd>
        </div>
        <div class="grid min-w-0 gap-1">
          <dt class="text-muted-foreground text-xs">
            {{ $t("configSourceRevision") }}
          </dt>
          <dd class="m-0">
            <code class="text-xs">{{ configPlan.expectedRevision }}</code>
          </dd>
        </div>
      </dl>
      <p class="text-muted-foreground m-0 text-xs">
        {{ $t("configCandidateSource") }}
      </p>
      <pre
        v-if="configPlan.diffRedacted"
        class="bg-background border-border m-0 max-h-80 overflow-auto rounded-md border p-3 text-xs"
        >{{ configPlan.diffRedacted }}</pre>
      <ul
        v-if="configPlan.warnings.length"
        class="m-0 list-disc pl-5 text-sm text-amber-900"
      >
        <li v-for="(warning, index) in configPlan.warnings" :key="index">
          {{ warning }}
        </li>
      </ul>
      <p
        v-if="actionExplanation('config.apply')"
        class="text-muted-foreground m-0 text-xs"
      >
        {{ $t("apply") }}: {{ actionExplanation("config.apply") }}
      </p>
      <template v-if="configPlan.validation === 'valid'">
        <FormField id="config-apply-reason" :label="$t('reason')">
          <Textarea
            id="config-apply-reason"
            v-model="configApplyReason"
            maxlength="512"
          />
        </FormField>
        <Button
          type="button"
          class="w-fit"
          :disabled="
            configLoading ||
            !canSubmitConfigPlan ||
            !actionAvailable('config.apply') ||
            !configApplyReason.trim()
          "
          @click="submitConfigApply"
        >
          {{ $t("apply") }}
        </Button>
      </template>
    </div>
    <p
      v-if="configOperation || configPlan?.operationId"
      class="m-0 flex flex-wrap items-center gap-2 text-sm"
    >
      {{ $t("operationId") }}:
      <code class="text-xs break-all">{{
        configOperation?.id ?? configPlan?.operationId
      }}</code>
      <span v-if="configOperation">{{
        $t(operationStatusKey(configOperation))
      }}</span>
    </p>
    <template #footer>
      <Button
        type="submit"
        :variant="configPlan ? 'outline' : 'default'"
        :disabled="
          configLoading ||
          !canSubmitConfigPlan ||
          !actionAvailable('config.plan') ||
          !configReason.trim()
        "
      >
        {{ $t("plan") }}
      </Button>
    </template>
  </OperationDialog>
</template>
