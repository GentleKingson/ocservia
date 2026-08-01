import {
  Configuration,
  OperationsApi,
  PlatformApi,
  type OperationPage,
  type Readiness,
} from "@ocservia/api-client";

const configuration = new Configuration({ basePath: "/api/v1" });
const operations = new OperationsApi(configuration);
const platform = new PlatformApi(configuration);

export async function listOperations(): Promise<OperationPage> {
  return operations.listOperations({});
}

export async function getReadiness(): Promise<Readiness> {
  return platform.getReadiness();
}
