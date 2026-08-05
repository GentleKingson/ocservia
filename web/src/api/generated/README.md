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
  DevelopmentApi,
} from '@ocservia/api-client';
import type { CreateLocalSimulationRequest } from '@ocservia/api-client';

async function example() {
  console.log("🚀 Testing @ocservia/api-client SDK...");
  const api = new DevelopmentApi();

  const body = {
    // SimulationScenario
    simulationScenario: ...,
  } satisfies CreateLocalSimulationRequest;

  try {
    const data = await api.createLocalSimulation(body);
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

| Class            | Method                                                                         | HTTP request                                               | Description                                                        |
| ---------------- | ------------------------------------------------------------------------------ | ---------------------------------------------------------- | ------------------------------------------------------------------ |
| _DevelopmentApi_ | [**createLocalSimulation**](docs/DevelopmentApi.md#createlocalsimulation)      | **POST** /development/simulations                          | Start a side-effect-free local agent simulation                    |
| _DevelopmentApi_ | [**getDevelopmentRuntime**](docs/DevelopmentApi.md#getdevelopmentruntime)      | **GET** /development/runtime                               | Read bounded runtime counters for local resilience tests           |
| _EnrollmentApi_  | [**createEnrollmentToken**](docs/EnrollmentApi.md#createenrollmenttoken)       | **POST** /enrollment-tokens                                | Create a one-time enrollment token                                 |
| _EventsApi_      | [**listEvents**](docs/EventsApi.md#listevents)                                 | **GET** /events                                            | Rebuild platform events from durable state                         |
| _EventsApi_      | [**watchEvents**](docs/EventsApi.md#watchevents)                               | **GET** /events/stream                                     | Watch platform events with SSE resume support                      |
| _NodesApi_       | [**approveNode**](docs/NodesApi.md#approvenode)                                | **POST** /nodes/{node_id}/approval                         | Approve a pending node and bind its endpoint identity              |
| _NodesApi_       | [**getNode**](docs/NodesApi.md#getnode)                                        | **GET** /nodes/{node_id}                                   | Get current node observed state                                    |
| _NodesApi_       | [**listNodeIpBans**](docs/NodesApi.md#listnodeipbans)                          | **GET** /nodes/{node_id}/ip-bans                           | List current IP bans observed on a node                            |
| _NodesApi_       | [**listNodeSessions**](docs/NodesApi.md#listnodesessions)                      | **GET** /nodes/{node_id}/sessions                          | List current sessions observed on a node                           |
| _NodesApi_       | [**listNodeTelemetry**](docs/NodesApi.md#listnodetelemetry)                    | **GET** /nodes/{node_id}/telemetry                         | Query bounded raw or rolled-up node telemetry                      |
| _NodesApi_       | [**listNodes**](docs/NodesApi.md#listnodes)                                    | **GET** /nodes                                             | List node observed state                                           |
| _NodesApi_       | [**revokeNode**](docs/NodesApi.md#revokenode)                                  | **POST** /nodes/{node_id}/revocation                       | Revoke a node endpoint and close its current connection            |
| _OperationsApi_  | [**createSyntheticCommand**](docs/OperationsApi.md#createsyntheticcommand)     | **POST** /nodes/{node_id}/synthetic-commands               | Queue a side-effect-free typed synthetic command                   |
| _OperationsApi_  | [**disconnectNodeSession**](docs/OperationsApi.md#disconnectnodesession)       | **POST** /nodes/{node_id}/sessions/{session_id}:disconnect | Disconnect one observed session                                    |
| _OperationsApi_  | [**getOperation**](docs/OperationsApi.md#getoperation)                         | **GET** /operations/{operation_id}                         | Get an asynchronous operation                                      |
| _OperationsApi_  | [**getOperationQueueMetrics**](docs/OperationsApi.md#getoperationqueuemetrics) | **GET** /operations/queue-metrics                          | Get transactional outbox and command queue metrics                 |
| _OperationsApi_  | [**listOperations**](docs/OperationsApi.md#listoperations)                     | **GET** /operations                                        | List operations                                                    |
| _OperationsApi_  | [**reloadNodeService**](docs/OperationsApi.md#reloadnodeservice)               | **POST** /nodes/{node_id}/service:reload                   | Reload the fixed Ocserv service unit                               |
| _OperationsApi_  | [**removeNodeIpBan**](docs/OperationsApi.md#removenodeipban)                   | **POST** /nodes/{node_id}/ip-bans/{ip}:remove              | Remove one canonical IP address from the Ocserv ban list           |
| _OperationsApi_  | [**terminateNodeSession**](docs/OperationsApi.md#terminatenodesession)         | **POST** /nodes/{node_id}/sessions/{session_id}:terminate  | Terminate one observed session and invalidate its reconnect cookie |
| _OperationsApi_  | [**watchOperationEvents**](docs/OperationsApi.md#watchoperationevents)         | **GET** /operations/{operation_id}/events                  | Watch durable operation state changes                              |
| _PlatformApi_    | [**getLiveness**](docs/PlatformApi.md#getliveness)                             | **GET** /livez                                             | Get process liveness                                               |
| _PlatformApi_    | [**getReadiness**](docs/PlatformApi.md#getreadiness)                           | **GET** /readyz                                            | Get dependency readiness                                           |
| _PlatformApi_    | [**getVersion**](docs/PlatformApi.md#getversion)                               | **GET** /version                                           | Get build metadata                                                 |

### Models

- [BuildInfo](docs/BuildInfo.md)
- [ConnectionPathState](docs/ConnectionPathState.md)
- [ControlledOperationRequest](docs/ControlledOperationRequest.md)
- [DevelopmentRuntime](docs/DevelopmentRuntime.md)
- [DropCounters](docs/DropCounters.md)
- [EnrollmentToken](docs/EnrollmentToken.md)
- [EnrollmentTokenRequest](docs/EnrollmentTokenRequest.md)
- [Health](docs/Health.md)
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
- [PlatformEvent](docs/PlatformEvent.md)
- [PlatformEventPage](docs/PlatformEventPage.md)
- [PlatformEventType](docs/PlatformEventType.md)
- [Problem](docs/Problem.md)
- [QueueMetrics](docs/QueueMetrics.md)
- [Readiness](docs/Readiness.md)
- [SimulationScenario](docs/SimulationScenario.md)
- [SyntheticCommandRequest](docs/SyntheticCommandRequest.md)
- [TelemetryMetric](docs/TelemetryMetric.md)
- [TelemetryPoint](docs/TelemetryPoint.md)
- [TelemetryPointPage](docs/TelemetryPointPage.md)

### Authorization

Authentication schemes defined for the API:
<a id="oidc"></a>

#### oidc

- **Type**: HTTP Bearer Token authentication (JWT)

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
