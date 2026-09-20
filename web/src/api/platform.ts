import {
  PlatformApi,
  type Readiness,
  type BuildInfo,
} from "@ocservia/api-client";
import { configuration } from "./transport";

const platform = new PlatformApi(configuration);
let authenticationProbe: Promise<void> | undefined;

export async function getReadiness(): Promise<Readiness> {
  return platform.getReadiness();
}

export async function getVersion(): Promise<BuildInfo> {
  return platform.getVersion();
}

export async function probeAuthentication(): Promise<void> {
  authenticationProbe ??= platform
    .listAuthorizedWorkspaces()
    .then(() => undefined)
    .finally(() => {
      authenticationProbe = undefined;
    });
  return authenticationProbe;
}
