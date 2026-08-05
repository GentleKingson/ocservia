import { readFile } from "node:fs/promises";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";
import { parse } from "yaml";

interface OpenApiDocument {
  openapi?: unknown;
  paths?: Record<string, Record<string, unknown>>;
  components?: {
    securitySchemes?: {
      oidc?: { type?: unknown; scheme?: unknown };
    };
    schemas?: {
      UuidV7?: { pattern?: unknown };
      Problem?: { required?: unknown };
      EnrollmentToken?: {
        properties?: {
          token?: { readOnly?: unknown; writeOnly?: unknown };
        };
      };
    };
  };
}

describe("OpenAPI invariants", () => {
  it("pins OpenAPI and the cross-language scalar conventions", async () => {
    const source = await readFile(
      resolve(import.meta.dirname, "../../openapi/openapi.yaml"),
      "utf8",
    );
    const document = parse(source) as OpenApiDocument;

    expect(document.openapi).toBe("3.1.2");
    expect(document.components?.schemas?.UuidV7?.pattern).toContain("-7");
    expect(document.components?.schemas?.Problem?.required).toEqual([
      "type",
      "title",
      "status",
    ]);
    expect(
      document.components?.schemas?.EnrollmentToken?.properties?.token,
    ).toMatchObject({ readOnly: true });
    expect(
      document.components?.schemas?.EnrollmentToken?.properties?.token
        ?.writeOnly,
    ).toBeUndefined();
    expect(document.components?.securitySchemes?.oidc).toMatchObject({
      type: "http",
      scheme: "bearer",
    });
  });

  it("publishes only the four typed controlled operation routes", async () => {
    const source = await readFile(
      resolve(import.meta.dirname, "../../openapi/openapi.yaml"),
      "utf8",
    );
    const document = parse(source) as OpenApiDocument;

    for (const path of [
      "/nodes/{node_id}/sessions/{session_id}:disconnect",
      "/nodes/{node_id}/sessions/{session_id}:terminate",
      "/nodes/{node_id}/ip-bans/{ip}:remove",
      "/nodes/{node_id}/service:reload",
    ]) {
      expect(document.paths?.[path]?.post).toBeDefined();
    }
    expect(source).not.toMatch(
      /shell|docker\.sock|systemctl_command|occtl_command/,
    );
  });
});
