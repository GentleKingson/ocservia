import { nextTick, reactive } from "vue";
import type { Certificate, ConfigPlan, Operation } from "@/api/models";
import { expect, vi } from "vitest";
import { stubSessionStorage } from "./helpers";
import { mountNodeDetail } from "./node-detail-renderer";

type AnyMock = ReturnType<typeof vi.fn>;

export interface NodeDetailViewState {
  configuration: {
    configDialog: boolean;
    configLoading: boolean;
    configError: string;
    configPlan?: ConfigPlan;
    configOperation?: Operation;
    configReason: string;
    configApplyReason: string;
    openConfigPlan(): Promise<void>;
    submitConfigPlan(): Promise<void>;
    submitConfigApply(): Promise<void>;
  };
  certificates: {
    certificateDialog: boolean;
    certificateLoading: boolean;
    certificateError: string;
    certificate?: Certificate;
    certificateOperation?: Operation;
    certificateGrant?: { downloadToken?: string; password?: string };
    certificateCommonName: string;
    certificateReason: string;
    openCertificate(): Promise<void>;
    submitCertificateRequest(): Promise<void>;
    submitCertificateIssue(): Promise<void>;
    createP12(): Promise<void>;
    downloadP12(): Promise<void>;
    revokeCurrentCertificate(): Promise<void>;
  };
  userGroup: {
    desiredDialog: { kind: string } | undefined;
    desiredPassword: string;
    desiredName: string;
    desiredReason: string;
    desiredLoading: boolean;
    desiredError: string;
    openDesired(kind: "create" | "rotate"): void;
    submitDesired(): Promise<void>;
  };
  page: {
    currentOperation: Operation | undefined;
    operationBusy: boolean;
    detailState: string;
    detailLoading: boolean;
    selectRouteNode(): Promise<void>;
    policyDialog: { username: string } | undefined;
    openPolicy(username: string): void;
  };
}

export interface NodeDetailMocks {
  fleet: Record<string, unknown>;
  route: { params: { nodeId: string } };
  cachedWorkspaceId: AnyMock;
  createConfigPlan: AnyMock;
  getConfigPlan: AnyMock;
  applyConfigPlan: AnyMock;
  listNodeCertificates: AnyMock;
  createCertificate: AnyMock;
  getCertificate: AnyMock;
  issueCertificate: AnyMock;
  createCertificateP12: AnyMock;
  revokeCertificate: AnyMock;
  downloadCertificateArtifact: AnyMock;
  getOperation: AnyMock;
  getUserPasswordSealingKey: AnyMock;
  getUserPolicy: AnyMock;
  setUserPolicy: AnyMock;
}

export const workflowPlan = {
  id: "plan-a",
  nodeId: "node-a",
  workspaceId: "workspace-a",
  operationId: "plan-operation",
  expectedRevision: 7,
  state: "succeeded",
  validation: "valid",
  warnings: [],
} as unknown as ConfigPlan;

export const workflowCertificate = {
  id: "certificate-a",
  nodeId: "node-a",
  workspaceId: "workspace-a",
  operationId: "csr-operation",
  version: 3,
  state: "csr_ready",
} as Certificate;

export const workflowOperation = {
  id: "operation-a",
  nodeId: "node-a",
  state: "succeeded",
} as Operation;

export const p12Grant = (
  overrides: Partial<{ downloadToken: string; password: string }> = {},
) => ({
  artifactId: "artifact-a",
  operation: workflowOperation,
  downloadToken: "one-time-token",
  password: "one-time-secret",
  expiresAt: new Date(Date.now() + 60_000).toISOString(),
  ...overrides,
});

export type CertMethod =
  | "submitCertificateRequest"
  | "submitCertificateIssue"
  | "createP12"
  | "revokeCurrentCertificate";

export function certApiFor(mocks: NodeDetailMocks, method: CertMethod) {
  return {
    submitCertificateRequest: mocks.createCertificate,
    submitCertificateIssue: mocks.issueCertificate,
    createP12: mocks.createCertificateP12,
    revokeCurrentCertificate: mocks.revokeCertificate,
  }[method];
}

export function useNodeDetailHarness(mocks: NodeDetailMocks) {
  let unmount: (() => void) | undefined;

  async function mount(): Promise<NodeDetailViewState> {
    mocks.fleet.selected = {
      id: "node-a",
      name: "Node A",
      version: 31,
      configRevision: 7,
    };
    const { app, instance } = mountNodeDetail();
    unmount = () => {
      app.unmount();
      unmount = undefined;
    };
    await nextTick();
    await nextTick();
    type Owner<K extends keyof NodeDetailViewState> = {
      $: { setupState: NodeDetailViewState[K] };
    };
    return {
      page: (instance as unknown as Owner<"page">).$.setupState,
      configuration: (instance.$refs.configuration as Owner<"configuration">).$
        .setupState,
      certificates: (instance.$refs.certificates as Owner<"certificates">).$
        .setupState,
      userGroup: (instance.$refs.userGroup as Owner<"userGroup">).$.setupState,
    };
  }

  function reset(): void {
    vi.useFakeTimers();
    vi.resetAllMocks();
    vi.stubGlobal("window", new EventTarget());
    stubSessionStorage();
    mocks.route = reactive({ params: { nodeId: "node-a" } });
    mocks.fleet = reactive({
      sessions: [],
      ipBans: [],
      initialized: true,
      selecting: false,
      selectionError: "",
      userGroupReadState: "ready",
      userGroupState: [],
      select: vi.fn().mockResolvedValue(undefined),
      start: vi.fn(),
      stop: vi.fn(),
      trackOperation: vi.fn().mockResolvedValue(undefined),
    });
    mocks.cachedWorkspaceId.mockReturnValue("workspace-a");
    mocks.createConfigPlan.mockResolvedValue(workflowPlan);
    mocks.getConfigPlan.mockResolvedValue(workflowPlan);
    mocks.applyConfigPlan.mockResolvedValue(workflowOperation);
    mocks.listNodeCertificates.mockResolvedValue([]);
    mocks.createCertificate.mockResolvedValue(workflowCertificate);
    mocks.getCertificate.mockResolvedValue(workflowCertificate);
    mocks.issueCertificate.mockResolvedValue({
      ...workflowCertificate,
      state: "issued",
    });
    mocks.createCertificateP12.mockResolvedValue(p12Grant());
    mocks.revokeCertificate.mockResolvedValue(workflowOperation);
    mocks.getOperation.mockResolvedValue(workflowOperation);
    mocks.getUserPolicy.mockResolvedValue(undefined);
    mocks.setUserPolicy.mockResolvedValue(undefined);
  }

  async function teardown(): Promise<void> {
    unmount?.();
    // Grants intentionally survive an SPA unmount, but expire in memory.
    await vi.advanceTimersByTimeAsync(10 * 60_000);
    expect(vi.getTimerCount()).toBe(0);
    vi.useRealTimers();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  }

  async function changeContext(
    view: NodeDetailViewState,
    kind: string,
  ): Promise<void> {
    if (kind === "node") {
      mocks.route.params.nodeId = "node-b";
      mocks.fleet.selected = {
        id: "node-b",
        name: "Node B",
        version: 31,
        configRevision: 7,
      };
    } else if (kind === "unmount") {
      unmount?.();
    } else {
      view.configuration.configDialog = false;
      view.certificates.certificateDialog = false;
    }
    await nextTick();
    await nextTick();
  }

  return {
    mount,
    reset,
    teardown,
    unmount: () => unmount?.(),
    changeContext,
  };
}
