import {
  Configuration,
  DevelopmentApi,
  EventsApi,
  OperationsApi,
  PlatformApi,
  type Operation,
  type OperationPage,
  type PlatformEventPage,
  type Readiness,
  type SimulationScenario,
} from "@ocservia/api-client";

const devAuthToken = import.meta.env.VITE_DEV_AUTH_TOKEN;
const configuration = new Configuration({
  basePath: "/api/v1",
  ...(devAuthToken ? { accessToken: devAuthToken } : {}),
});
const operations = new OperationsApi(configuration);
const platform = new PlatformApi(configuration);
const development = new DevelopmentApi(configuration);
const events = new EventsApi(configuration);

export async function listOperations(cursor?: string): Promise<OperationPage> {
  return operations.listOperations(
    cursor ? { cursor, pageSize: 200 } : { pageSize: 200 },
  );
}

export async function getReadiness(): Promise<Readiness> {
  return platform.getReadiness();
}

export async function createLocalSimulation(
  simulationScenario: SimulationScenario,
): Promise<Operation> {
  return development.createLocalSimulation({ simulationScenario });
}

export async function getOperation(operationId: string): Promise<Operation> {
  return operations.getOperation({ operationId });
}

export async function listEvents(after?: string): Promise<PlatformEventPage> {
  return events.listEvents(
    after ? { after, pageSize: 200 } : { pageSize: 200 },
  );
}
