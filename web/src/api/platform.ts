import { client, read } from "./transport";

let authenticationProbe: Promise<void> | undefined;

export function getReadiness() {
  return read(client.GET("/readyz"));
}

export function getVersion() {
  return read(client.GET("/version"));
}

export async function loginLocal(
  username: string,
  password: string,
): Promise<Response> {
  const { response } = await client.POST("/auth/login", {
    cache: "no-store",
    body: { username, password },
  });
  return response;
}

export function probeAuthentication(): Promise<void> {
  authenticationProbe ??= client
    .GET("/workspaces")
    .then(() => undefined)
    .finally(() => {
      authenticationProbe = undefined;
    });
  return authenticationProbe;
}
