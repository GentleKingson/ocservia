<script setup lang="ts">
import type { Ref } from "vue";
import { Download, KeyRound } from "@lucide/vue";
import type { NodeObservedState } from "@ocservia/api-client";
import { useI18n } from "vue-i18n";
import { formatTimestamp } from "@/shared/timestamp";
import { workspaceContext } from "@/api/workspace";
import { useNodeCertificates } from "@/features/certificates/useNodeCertificates";
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
  certificateDialog,
  certificate,
  certificateGrant,
  certificateOperation,
  certificateCommonName,
  certificateDnsNames,
  certificateReason,
  certificateError,
  certificateLoading,
  openCertificate,
  submitCertificateRequest,
  submitCertificateIssue,
  createP12,
  downloadP12,
  revokeCurrentCertificate,
} = useNodeCertificates({
  currentNode,
  readReady: props.context.readReady,
  workspaceContext,
  trackOperation: (id) => fleet.trackOperation(id),
  t,
});

function close() {
  certificateDialog.value = false;
}
defineExpose({ open: openCertificate, close });
</script>
<template>
  <OperationDialog
    v-if="certificateDialog"
    :title="$t('certificateLifecycle')"
    :subject="currentNode?.name"
    :error="certificateError"
    @close="certificateDialog = false"
    @submit="submitCertificateRequest"
  >
    <p
      v-if="!actionAvailable('certificate.issue')"
      class="text-muted-foreground m-0 text-xs"
    >
      {{ $t("requestCsr") }}: {{ actionExplanation("certificate.issue") }}
    </p>
    <template v-if="!certificate && actionAvailable('certificate.issue')">
      <FormField id="certificate-cn" :label="$t('commonName')">
        <Input
          id="certificate-cn"
          v-model="certificateCommonName"
          maxlength="253"
          required
        />
      </FormField>
      <FormField id="certificate-dns" :label="$t('dnsNames')">
        <Input
          id="certificate-dns"
          v-model="certificateDnsNames"
          maxlength="4096"
        />
      </FormField>
    </template>
    <template v-else-if="certificate">
      <div
        class="bg-muted/50 border-border flex min-w-0 flex-wrap items-center gap-2 rounded-md border p-3 text-sm"
        aria-live="polite"
      >
        <Badge
          variant="outline"
          :class="
            certificate.state === 'issued'
              ? 'text-success'
              : certificate.state === 'expiring'
                ? 'text-amber-800'
                : undefined
          "
          >{{ certificate.state }}</Badge
        >
        <code class="min-w-0 text-xs break-all">{{ certificate.id }}</code>
        <small v-if="certificate.notAfter" class="text-muted-foreground"
          >{{ $t("expires") }}
          {{ formatTimestamp(certificate.notAfter) }}</small
        >
      </div>
      <FormField
        v-if="certificateGrant?.password"
        id="certificate-password"
        :label="$t('p12Password')"
        :help="$t('oneTimeCredential')"
        v-slot="{ describedBy }"
      >
        <Input
          id="certificate-password"
          class="font-mono"
          :model-value="certificateGrant.password"
          :aria-describedby="describedBy"
          readonly
          autocomplete="off"
        />
      </FormField>
    </template>
    <FormField id="certificate-reason" :label="$t('reason')">
      <Textarea
        id="certificate-reason"
        v-model="certificateReason"
        maxlength="512"
        required
      />
    </FormField>
    <p
      v-if="certificateOperation || certificate?.operationId"
      class="m-0 flex flex-wrap items-center gap-2 text-sm"
    >
      {{ $t("operationId") }}:
      <code class="text-xs break-all">{{
        certificateOperation?.id ?? certificate?.operationId
      }}</code>
      <span v-if="certificateOperation">{{
        $t(operationStatusKey(certificateOperation))
      }}</span>
    </p>
    <template #footer>
      <Button
        v-if="!certificate"
        type="submit"
        :disabled="
          certificateLoading ||
          !actionAvailable('certificate.issue') ||
          !certificateReason.trim()
        "
      >
        {{ $t("requestCsr") }}
      </Button>
      <Button
        v-else-if="
          certificate.state === 'csr_ready' ||
          certificate.state === 'signer_unavailable'
        "
        type="button"
        :disabled="
          certificateLoading ||
          !actionAvailable('certificate.issue') ||
          !certificateReason.trim()
        "
        @click="submitCertificateIssue"
      >
        {{ $t("issueCertificate") }}
      </Button>
      <template
        v-else-if="
          certificate.state === 'issued' || certificate.state === 'expiring'
        "
      >
        <Button
          type="button"
          variant="outline"
          :disabled="
            certificateLoading ||
            !actionAvailable('certificate.private_key.export') ||
            Boolean(certificateGrant?.downloadToken) ||
            !certificateReason.trim()
          "
          @click="createP12"
        >
          <KeyRound aria-hidden="true" />{{ $t("createP12") }}
        </Button>
        <Button
          v-if="certificateGrant?.downloadToken"
          type="button"
          :disabled="certificateLoading"
          @click="downloadP12"
        >
          <Download aria-hidden="true" />{{ $t("download") }}
        </Button>
        <Button
          type="button"
          variant="destructive"
          :disabled="
            certificateLoading ||
            !actionAvailable('certificate.revoke') ||
            !certificateReason.trim()
          "
          @click="revokeCurrentCertificate"
        >
          {{ $t("revoke") }}
        </Button>
      </template>
    </template>
  </OperationDialog>
</template>
