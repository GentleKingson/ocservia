import type { components } from "./generated/schema";

export type Schemas = components["schemas"];

type CamelKey<Key extends string> = Key extends `${infer Head}_${infer Tail}`
  ? `${Head}${Capitalize<CamelKey<Tail>>}`
  : Key;

// These fields carry keys or values defined outside the OpenAPI object shape.
const opaqueKeys = [
  "ocserv",
  "system",
  "effective_actions",
  "detail",
  "warnings",
] as const;
type OpaqueKey = (typeof opaqueKeys)[number];
const opaqueFields = new Set<string>(opaqueKeys);

type Camel<T> = T extends readonly (infer Item)[]
  ? Camel<Item>[]
  : T extends object
    ? {
        [Key in keyof T as CamelKey<Key & string>]: Key extends OpaqueKey
          ? T[Key]
          : Camel<T[Key]>;
      }
    : T;

function camelize(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(camelize);
  if (value === null || typeof value !== "object") return value;
  return Object.fromEntries(
    Object.entries(value).map(([key, item]) => [
      key.replace(/_([a-z])/g, (_, letter: string) => letter.toUpperCase()),
      opaqueFields.has(key) ? item : camelize(item),
    ]),
  );
}

export function fromWire<T>(value: T): Camel<T> {
  return camelize(value) as Camel<T>;
}

export type AgentRollout = Camel<Schemas["AgentRollout"]>;
export type AgentRolloutNode = Camel<Schemas["AgentRolloutNode"]>;
export type ArtifactGrant = Camel<Schemas["ArtifactGrant"]>;
export type AuditEventPage = Camel<Schemas["AuditEventPage"]>;
export type AuditVerification = Camel<Schemas["AuditVerification"]>;
export type BuildInfo = Camel<Schemas["BuildInfo"]>;
export type Certificate = Camel<Schemas["Certificate"]>;
export type CertificateIssueRequest = Camel<Schemas["CertificateIssueRequest"]>;
export type CertificateP12Request = Camel<Schemas["CertificateP12Request"]>;
export type CertificateRequest = Camel<Schemas["CertificateRequest"]>;
export type CertificateRevokeRequest = Camel<
  Schemas["CertificateRevokeRequest"]
>;
export type ConfigApplyRequest = Camel<Schemas["ConfigApplyRequest"]>;
export type ConfigPlan = Camel<Schemas["ConfigPlan"]>;
export type ConfigPlanRequest = Camel<Schemas["ConfigPlanRequest"]>;
export type NodeIpBan = Camel<Schemas["NodeIpBan"]>;
export type NodeObservedState = Camel<Schemas["NodeObservedState"]>;
export type NodeSession = Camel<Schemas["NodeSession"]>;
export type Operation = Camel<Schemas["Operation"]>;
export type OperationPage = Camel<Schemas["OperationPage"]>;
export type OperationSummary = Camel<Schemas["OperationSummary"]>;
export type PlatformEvent = Camel<Schemas["PlatformEvent"]>;
export type PlatformEventPage = Camel<Schemas["PlatformEventPage"]>;
export type Readiness = Camel<Schemas["Readiness"]>;
export type UserGroupResourceState = Camel<Schemas["UserGroupResourceState"]>;
export type UserPasswordSealingKey = Camel<Schemas["UserPasswordSealingKey"]>;
export type UserPolicy = Camel<Schemas["UserPolicy"]>;
export type UserPolicyRequest = Camel<Schemas["UserPolicyRequest"]>;
export type VpnAuditRecord = Camel<Schemas["VpnAuditRecord"]>;
export type Workspace = Camel<Schemas["Workspace"]>;
