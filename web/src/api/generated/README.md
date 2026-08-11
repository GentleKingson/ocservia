# @ocservia/api-client@0.1.0

A TypeScript SDK client for the localhost API.

## Usage

First, install the SDK from npm.

```bash
npm install @ocservia/api-client --save
```

Next, try it out.

```ts
import {
  Configuration,
  CertificatesApi,
} from '@ocservia/api-client';
import type { CreateCertificateRequest } from '@ocservia/api-client';

async function example() {
  console.log("🚀 Testing @ocservia/api-client SDK...");
  const config = new Configuration({
    // To configure API key authorization: oidc
    apiKey: "YOUR API KEY",
    // Configure HTTP bearer authorization: bearerAuth
    accessToken: "YOUR BEARER TOKEN",
  });
  const api = new CertificatesApi(config);

  const body = {
    // string
    nodeId: 38400000-8cf0-11bd-b23e-10b96e4ef00d,
    // string
    idempotencyKey: idempotencyKey_example,
    // CertificateRequest
    certificateRequest: ...,
  } satisfies CreateCertificateRequest;

  try {
    const data = await api.createCertificate(body);
    console.log(data);
  } catch (error) {
    console.error(error);
  }
}

// Run the test
example().catch(console.error);
```

## Documentation

### API Endpoints

All URIs are relative to _/api/v1_

| Class              | Method                                                                         | HTTP request                                               | Description                                                                      |
| ------------------ | ------------------------------------------------------------------------------ | ---------------------------------------------------------- | -------------------------------------------------------------------------------- |
| _CertificatesApi_  | [**createCertificate**](docs/CertificatesApi.md#createcertificate)             | **POST** /nodes/{node_id}/certificates                     | Generate a node-local private key and queue a CSR operation                      |
| _CertificatesApi_  | [**createCertificateP12**](docs/CertificatesApi.md#createcertificatep12)       | **POST** /certificates/{certificate_id}:p12                | Create a short-lived one-time encrypted P12 artifact                             |
| _CertificatesApi_  | [**createSecretProviderRef**](docs/CertificatesApi.md#createsecretproviderref) | **POST** /secret-provider-refs                             | Register opaque external secret metadata without a secret value                  |
| _CertificatesApi_  | [**downloadArtifact**](docs/CertificatesApi.md#downloadartifact)               | **GET** /artifacts/{artifact_id}                           | Stream one ready artifact once with its separate bearer token                    |
| _CertificatesApi_  | [**getCertificate**](docs/CertificatesApi.md#getcertificate)                   | **GET** /certificates/{certificate_id}                     | Get node-scoped certificate lifecycle state and public metadata                  |
| _CertificatesApi_  | [**getSecretProviderRef**](docs/CertificatesApi.md#getsecretproviderref)       | **GET** /secret-provider-refs/{secret_ref_id}              | Read opaque provider/version metadata                                            |
| _CertificatesApi_  | [**issueCertificate**](docs/CertificatesApi.md#issuecertificate)               | **POST** /certificates/{certificate_id}:issue              | Ask the configured external PKI signer to issue a ready CSR                      |
| _CertificatesApi_  | [**listNodeCertificates**](docs/CertificatesApi.md#listnodecertificates)       | **GET** /nodes/{node_id}/certificates                      | List the newest node-scoped certificate lifecycle records                        |
| _CertificatesApi_  | [**revokeCertificate**](docs/CertificatesApi.md#revokecertificate)             | **POST** /certificates/{certificate_id}:revoke             | Revoke an issued certificate and remove its node-local private key               |
| _CertificatesApi_  | [**rotateSecretProviderRef**](docs/CertificatesApi.md#rotatesecretproviderref) | **POST** /secret-provider-refs/{secret_ref_id}:rotate      | Record an externally completed secret version rotation                           |
| _ConfigurationApi_ | [**applyConfigPlan**](docs/ConfigurationApi.md#applyconfigplan)                | **POST** /config-plans/{plan_id}/apply                     | Queue atomic application of an unexpired independently approved plan             |
| _ConfigurationApi_ | [**createConfigPlan**](docs/ConfigurationApi.md#createconfigplan)              | **POST** /nodes/{node_id}/config-plans                     | Render and remotely validate an immutable configuration candidate                |
| _ConfigurationApi_ | [**getConfigPlan**](docs/ConfigurationApi.md#getconfigplan)                    | **GET** /config-plans/{plan_id}                            | Get validation, safe diff, warnings, and approval state for a configuration plan |
| _DevelopmentApi_   | [**createLocalSimulation**](docs/DevelopmentApi.md#createlocalsimulation)      | **POST** /development/simulations                          | Start a side-effect-free local agent simulation                                  |
| _DevelopmentApi_   | [**getDevelopmentRuntime**](docs/DevelopmentApi.md#getdevelopmentruntime)      | **GET** /development/runtime                               | Read bounded runtime counters for local resilience tests                         |
| _EnrollmentApi_    | [**createEnrollmentToken**](docs/EnrollmentApi.md#createenrollmenttoken)       | **POST** /enrollment-tokens                                | Create a one-time enrollment token                                               |
| _EventsApi_        | [**listAuditEvents**](docs/EventsApi.md#listauditevents)                       | **GET** /audit/events                                      | List append-only audit events                                                    |
| _EventsApi_        | [**listEvents**](docs/EventsApi.md#listevents)                                 | **GET** /events                                            | Rebuild platform events from durable state                                       |
| _EventsApi_        | [**verifyAuditChain**](docs/EventsApi.md#verifyauditchain)                     | **POST** /audit:verify                                     | Verify the workspace audit hash chain and latest checkpoint                      |
| _EventsApi_        | [**watchEvents**](docs/EventsApi.md#watchevents)                               | **GET** /events/stream                                     | Watch platform events with SSE resume support                                    |
| _NodesApi_         | [**approveNode**](docs/NodesApi.md#approvenode)                                | **POST** /nodes/{node_id}/approval                         | Approve a pending node and bind its endpoint identity                            |
| _NodesApi_         | [**getNode**](docs/NodesApi.md#getnode)                                        | **GET** /nodes/{node_id}                                   | Get current node observed state                                                  |
| _NodesApi_         | [**getNodeUserPolicy**](docs/NodesApi.md#getnodeuserpolicy)                    | **GET** /nodes/{node_id}/users/{username}/policy           | Get desired quota and UTC expiry with observed usage                             |
| _NodesApi_         | [**listNodeIpBans**](docs/NodesApi.md#listnodeipbans)                          | **GET** /nodes/{node_id}/ip-bans                           | List current IP bans observed on a node                                          |
| _NodesApi_         | [**listNodeSessions**](docs/NodesApi.md#listnodesessions)                      | **GET** /nodes/{node_id}/sessions                          | List current sessions observed on a node                                         |
| _NodesApi_         | [**listNodeTelemetry**](docs/NodesApi.md#listnodetelemetry)                    | **GET** /nodes/{node_id}/telemetry                         | Query bounded raw or rolled-up node telemetry                                    |
| _NodesApi_         | [**listNodeUserGroupState**](docs/NodesApi.md#listnodeusergroupstate)          | **GET** /nodes/{node_id}/user-group-state                  | List node-scoped desired and observed users and groups                           |
| _NodesApi_         | [**listNodes**](docs/NodesApi.md#listnodes)                                    | **GET** /nodes                                             | List node observed state                                                         |
| _NodesApi_         | [**revokeNode**](docs/NodesApi.md#revokenode)                                  | **POST** /nodes/{node_id}/revocation                       | Revoke a node endpoint and close its current connection                          |
| _OperationsApi_    | [**applyNodeGroup**](docs/OperationsApi.md#applynodegroup)                     | **PUT** /nodes/{node_id}/groups/{group_name}               | Queue atomic application of one node-scoped group                                |
| _OperationsApi_    | [**approveRequest**](docs/OperationsApi.md#approverequest)                     | **POST** /approval-requests/{approval_id}:approve          | Approve another principal\&#39;s high-risk request                               |
| _OperationsApi_    | [**createApprovalRequest**](docs/OperationsApi.md#createapprovalrequest)       | **POST** /approval-requests                                | Request independent approval for a high-risk action                              |
| _OperationsApi_    | [**createNodeUser**](docs/OperationsApi.md#createnodeuser)                     | **POST** /nodes/{node_id}/users                            | Queue creation of one node-scoped user                                           |
| _OperationsApi_    | [**createSyntheticCommand**](docs/OperationsApi.md#createsyntheticcommand)     | **POST** /nodes/{node_id}/synthetic-commands               | Queue a side-effect-free typed synthetic command                                 |
| _OperationsApi_    | [**createUserBatch**](docs/OperationsApi.md#createuserbatch)                   | **POST** /user-batches                                     | Create a parent batch whose items are authorized and executed independently      |
| _OperationsApi_    | [**disableNodeUser**](docs/OperationsApi.md#disablenodeuser)                   | **POST** /nodes/{node_id}/users/{username}:disable         | Queue disabling one node-scoped user                                             |
| _OperationsApi_    | [**disconnectNodeSession**](docs/OperationsApi.md#disconnectnodesession)       | **POST** /nodes/{node_id}/sessions/{session_id}:disconnect | Disconnect one observed session                                                  |
| _OperationsApi_    | [**enableNodeUser**](docs/OperationsApi.md#enablenodeuser)                     | **POST** /nodes/{node_id}/users/{username}:enable          | Queue enabling one node-scoped user without changing its password or groups      |
| _OperationsApi_    | [**getApprovalRequest**](docs/OperationsApi.md#getapprovalrequest)             | **GET** /approval-requests/{approval_id}                   | Inspect immutable approval details before deciding                               |
| _OperationsApi_    | [**getOperation**](docs/OperationsApi.md#getoperation)                         | **GET** /operations/{operation_id}                         | Get an asynchronous operation                                                    |
| _OperationsApi_    | [**getOperationQueueMetrics**](docs/OperationsApi.md#getoperationqueuemetrics) | **GET** /operations/queue-metrics                          | Get transactional outbox and command queue metrics                               |
| _OperationsApi_    | [**getUserBatch**](docs/OperationsApi.md#getuserbatch)                         | **GET** /user-batches/{batch_id}                           | Get parent and per-item authorization, command, and result state                 |
| _OperationsApi_    | [**getUserOperationMetrics**](docs/OperationsApi.md#getuseroperationmetrics)   | **GET** /user-operations/metrics                           | Get workspace quota, expiry, and batch scheduler health counters                 |
| _OperationsApi_    | [**listOperations**](docs/OperationsApi.md#listoperations)                     | **GET** /operations                                        | List operations                                                                  |
| _OperationsApi_    | [**reloadNodeService**](docs/OperationsApi.md#reloadnodeservice)               | **POST** /nodes/{node_id}/service:reload                   | Reload the fixed Ocserv service unit                                             |
| _OperationsApi_    | [**removeNodeIpBan**](docs/OperationsApi.md#removenodeipban)                   | **POST** /nodes/{node_id}/ip-bans/{ip}:remove              | Remove one canonical IP address from the Ocserv ban list                         |
| _OperationsApi_    | [**rotateNodeUserPassword**](docs/OperationsApi.md#rotatenodeuserpassword)     | **POST** /nodes/{node_id}/users/{username}:rotate-password | Queue write-only password rotation using node-sealed ciphertext                  |
| _OperationsApi_    | [**setNodeUserPolicy**](docs/OperationsApi.md#setnodeuserpolicy)               | **PUT** /nodes/{node_id}/users/{username}/policy           | Set byte quota and exact UTC expiry for one node-scoped user                     |
| _OperationsApi_    | [**terminateNodeSession**](docs/OperationsApi.md#terminatenodesession)         | **POST** /nodes/{node_id}/sessions/{session_id}:terminate  | Terminate one observed session and invalidate its reconnect cookie               |
| _OperationsApi_    | [**watchOperationEvents**](docs/OperationsApi.md#watchoperationevents)         | **GET** /operations/{operation_id}/events                  | Watch durable operation state changes                                            |
| _PlatformApi_      | [**beginOIDCLogin**](docs/PlatformApi.md#beginoidclogin)                       | **GET** /auth/login                                        | Begin OIDC Authorization Code login with PKCE S256                               |
| _PlatformApi_      | [**completeOIDCLogin**](docs/PlatformApi.md#completeoidclogin)                 | **GET** /auth/callback                                     | Validate OIDC state, nonce, code, and ID token                                   |
| _PlatformApi_      | [**createRoleBinding**](docs/PlatformApi.md#createrolebinding)                 | **POST** /role-bindings                                    | Bind one baseline role to a workspace or resource scope                          |
| _PlatformApi_      | [**getLiveness**](docs/PlatformApi.md#getliveness)                             | **GET** /livez                                             | Get process liveness                                                             |
| _PlatformApi_      | [**getReadiness**](docs/PlatformApi.md#getreadiness)                           | **GET** /readyz                                            | Get dependency readiness                                                         |
| _PlatformApi_      | [**getVersion**](docs/PlatformApi.md#getversion)                               | **GET** /version                                           | Get build metadata                                                               |
| _PlatformApi_      | [**listAuthorizedWorkspaces**](docs/PlatformApi.md#listauthorizedworkspaces)   | **GET** /workspaces                                        | List only workspaces visible to the current principal                            |
| _PlatformApi_      | [**logout**](docs/PlatformApi.md#logout)                                       | **POST** /auth/logout                                      | Revoke the current server-side session                                           |
| _PlatformApi_      | [**useBreakGlass**](docs/PlatformApi.md#usebreakglass)                         | **POST** /auth/break-glass                                 | Use explicitly enabled offline emergency access                                  |

### Models

- [Approval](docs/Approval.md)
- [ApprovalDecision](docs/ApprovalDecision.md)
- [ApprovalRequest](docs/ApprovalRequest.md)
- [ApprovalRequestCertificate](docs/ApprovalRequestCertificate.md)
- [ApprovalRequestRoleBinding](docs/ApprovalRequestRoleBinding.md)
- [ApprovalRequestSummary](docs/ApprovalRequestSummary.md)
- [ArtifactGrant](docs/ArtifactGrant.md)
- [AuditEventPage](docs/AuditEventPage.md)
- [AuditVerification](docs/AuditVerification.md)
- [BreakGlassRequest](docs/BreakGlassRequest.md)
- [BuildInfo](docs/BuildInfo.md)
- [Certificate](docs/Certificate.md)
- [CertificateActionApprovalSummary](docs/CertificateActionApprovalSummary.md)
- [CertificateApprovalSummary](docs/CertificateApprovalSummary.md)
- [CertificateIssueApprovalSummary](docs/CertificateIssueApprovalSummary.md)
- [CertificateIssueRequest](docs/CertificateIssueRequest.md)
- [CertificateP12Request](docs/CertificateP12Request.md)
- [CertificateRequest](docs/CertificateRequest.md)
- [CertificateRevokeRequest](docs/CertificateRevokeRequest.md)
- [ConfigApplyRequest](docs/ConfigApplyRequest.md)
- [ConfigDirective](docs/ConfigDirective.md)
- [ConfigPlan](docs/ConfigPlan.md)
- [ConfigPlanApprovalSummary](docs/ConfigPlanApprovalSummary.md)
- [ConfigPlanRequest](docs/ConfigPlanRequest.md)
- [ConfigSecretRef](docs/ConfigSecretRef.md)
- [ConfigTemplate](docs/ConfigTemplate.md)
- [ConnectionPathState](docs/ConnectionPathState.md)
- [ControlledOperationRequest](docs/ControlledOperationRequest.md)
- [DesiredMutationRequest](docs/DesiredMutationRequest.md)
- [DevelopmentRuntime](docs/DevelopmentRuntime.md)
- [DropCounters](docs/DropCounters.md)
- [EnrollmentToken](docs/EnrollmentToken.md)
- [EnrollmentTokenRequest](docs/EnrollmentTokenRequest.md)
- [GroupApplyRequest](docs/GroupApplyRequest.md)
- [Health](docs/Health.md)
- [ListNodeCertificates200Response](docs/ListNodeCertificates200Response.md)
- [NodeApproval](docs/NodeApproval.md)
- [NodeIpBan](docs/NodeIpBan.md)
- [NodeIpBanPage](docs/NodeIpBanPage.md)
- [NodeObservedState](docs/NodeObservedState.md)
- [NodePage](docs/NodePage.md)
- [NodeRevocation](docs/NodeRevocation.md)
- [NodeSession](docs/NodeSession.md)
- [NodeSessionPage](docs/NodeSessionPage.md)
- [NodeTrustState](docs/NodeTrustState.md)
- [Operation](docs/Operation.md)
- [OperationPage](docs/OperationPage.md)
- [OperationState](docs/OperationState.md)
- [PageInfo](docs/PageInfo.md)
- [PasswordRotateRequest](docs/PasswordRotateRequest.md)
- [PlatformEvent](docs/PlatformEvent.md)
- [PlatformEventPage](docs/PlatformEventPage.md)
- [PlatformEventType](docs/PlatformEventType.md)
- [Problem](docs/Problem.md)
- [QueueMetrics](docs/QueueMetrics.md)
- [Readiness](docs/Readiness.md)
- [RoleBinding](docs/RoleBinding.md)
- [RoleBindingRequest](docs/RoleBindingRequest.md)
- [SecretProviderRef](docs/SecretProviderRef.md)
- [SecretProviderRefRequest](docs/SecretProviderRefRequest.md)
- [SecretProviderRotateRequest](docs/SecretProviderRotateRequest.md)
- [SimulationScenario](docs/SimulationScenario.md)
- [SyntheticCommandRequest](docs/SyntheticCommandRequest.md)
- [TelemetryMetric](docs/TelemetryMetric.md)
- [TelemetryPoint](docs/TelemetryPoint.md)
- [TelemetryPointPage](docs/TelemetryPointPage.md)
- [UserBatch](docs/UserBatch.md)
- [UserBatchItem](docs/UserBatchItem.md)
- [UserBatchItemRequest](docs/UserBatchItemRequest.md)
- [UserBatchRequest](docs/UserBatchRequest.md)
- [UserCreateRequest](docs/UserCreateRequest.md)
- [UserGroupResourceState](docs/UserGroupResourceState.md)
- [UserGroupStatePage](docs/UserGroupStatePage.md)
- [UserOperationMetrics](docs/UserOperationMetrics.md)
- [UserPasswordSealedSecretV1](docs/UserPasswordSealedSecretV1.md)
- [UserPolicy](docs/UserPolicy.md)
- [UserPolicyRequest](docs/UserPolicyRequest.md)
- [Workspace](docs/Workspace.md)
- [WorkspacePage](docs/WorkspacePage.md)

### Authorization

Authentication schemes defined for the API:
<a id="oidc"></a>

#### oidc

- **Type**: API key
- **API key parameter name**: `__Host-ocservia_session`
- **Location**:
  <a id="bearerAuth"></a>

#### bearerAuth

- **Type**: HTTP Bearer Token authentication

## About

This TypeScript SDK client supports the [Fetch API](https://fetch.spec.whatwg.org/)
and is automatically generated by the
[OpenAPI Generator](https://openapi-generator.tech) project:

- API version: `1.0.0`
- Package version: `0.1.0`
- Generator version: `7.24.0`
- Build package: `org.openapitools.codegen.languages.TypeScriptFetchClientCodegen`

The generated npm module supports the following:

- Environments
  - Node.js
  - Webpack
  - Browserify
- Language levels
  - ES5 - you must have a Promises/A+ library installed
  - ES6
- Module systems
  - CommonJS
  - ES6 module system

## Development

### Building

To build the TypeScript source code, you need to have Node.js and npm installed.
After cloning the repository, navigate to the project directory and run:

```bash
npm install
npm run build
```

### Publishing

Once you've built the package, you can publish it to npm:

```bash
npm publish
```

## License

[MIT OR Apache-2.0](<>)
