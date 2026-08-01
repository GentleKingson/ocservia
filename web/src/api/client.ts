import {
  Configuration,
  OperationsApi,
  type OperationPage,
} from "@ocservia/api-client";

const operations = new OperationsApi(
  new Configuration({ basePath: "/api/v1" }),
);

export async function listOperations(): Promise<OperationPage> {
  return operations.listOperations({});
}
