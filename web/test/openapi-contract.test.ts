import { readFile } from "node:fs/promises";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";
import { parse } from "yaml";

interface OpenApiDocument {
  openapi?: unknown;
  paths?: Record<string, Record<string, unknown>>;
  components?: {
    responses?: Record<string, { content?: Record<string, unknown> }>;
    securitySchemes?: {
      oidc?: { type?: unknown; in?: unknown; name?: unknown };
      bearerAuth?: { type?: unknown; scheme?: unknown };
    };
    schemas?: {
      UuidV7?: { pattern?: unknown };
      Problem?: { required?: unknown };
      EnrollmentToken?: {
        properties?: {
          token?: { readOnly?: unknown; writeOnly?: unknown };
        };
      };
      GroupApplyRequest?: {
        properties?: { members?: { maxItems?: unknown } };
      };
      UserGroupResourceState?: {
        required?: unknown;
        properties?: {
          desired_members?: { maxItems?: unknown };
          observed_members?: { maxItems?: unknown };
          recovery_required?: { type?: unknown };
          recovery_mutation_kind?: { enum?: unknown };
        };
      };
      UserGroupStatePage?: {
        properties?: { items?: { maxItems?: unknown } };
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
      type: "apiKey",
      in: "cookie",
      name: "__Host-ocservia_session",
    });
    expect(document.components?.securitySchemes?.bearerAuth).toMatchObject({
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

  it("publishes the role binding identifier returned by the server", async () => {
    const source = await readFile(
      resolve(import.meta.dirname, "../../openapi/openapi.yaml"),
      "utf8",
    );
    const document = parse(source) as OpenApiDocument;
    const response = document.paths?.["/role-bindings"]?.post as
      { responses?: Record<string, unknown> } | undefined;

    expect(response?.responses?.["201"]).toMatchObject({
      content: {
        "application/json": {
          schema: { $ref: "#/components/schemas/RoleBinding" },
        },
      },
    });
  });

  it("publishes the transport-safe user and group capacity", async () => {
    const source = await readFile(
      resolve(import.meta.dirname, "../../openapi/openapi.yaml"),
      "utf8",
    );
    const schemas = (parse(source) as OpenApiDocument).components?.schemas;

    expect(schemas?.GroupApplyRequest?.properties?.members?.maxItems).toBe(384);
    expect(
      schemas?.UserGroupResourceState?.properties?.desired_members?.maxItems,
    ).toBe(384);
    expect(
      schemas?.UserGroupResourceState?.properties?.observed_members?.maxItems,
    ).toBe(384);
    expect(schemas?.UserGroupStatePage?.properties?.items?.maxItems).toBe(1536);
    expect(schemas?.UserGroupResourceState?.required).toContain(
      "recovery_required",
    );
    expect(
      schemas?.UserGroupResourceState?.properties?.recovery_mutation_kind?.enum,
    ).toEqual([
      "user_create",
      "user_disable",
      "user_enable",
      "user_password_rotate",
      "group_apply",
    ]);
  });

  it("pins the browser trust boundary on every JSON request body", async () => {
    const source = await readFile(
      resolve(import.meta.dirname, "../../openapi/openapi.yaml"),
      "utf8",
    );
    const document = parse(source) as OpenApiDocument;
    const responses = document.components?.responses ?? {};

    expect(Object.keys(responses.CrossOriginRequest?.content ?? {})).toEqual([
      "application/problem+json",
    ]);
    expect(Object.keys(responses.UnsupportedMediaType?.content ?? {})).toEqual([
      "application/problem+json",
    ]);

    const methods = ["get", "post", "put", "patch", "delete"] as const;
    for (const item of Object.values(document.paths ?? {})) {
      for (const method of methods) {
        const operation = item[method] as
          | {
              requestBody?: { content?: Record<string, unknown> };
              responses?: Record<string, { $ref?: string }>;
            }
          | undefined;
        if (!operation?.requestBody) continue;
        expect(Object.keys(operation.requestBody.content ?? {})).toEqual([
          "application/json",
        ]);
        expect(operation.responses?.["415"]).toEqual({
          $ref: "#/components/responses/UnsupportedMediaType",
        });
      }
    }

    const breakGlass = document.paths?.["/auth/break-glass"]?.post as
      { responses?: Record<string, { $ref?: string }> } | undefined;
    expect(breakGlass?.responses?.["403"]).toEqual({
      $ref: "#/components/responses/CrossOriginRequest",
    });
  });
});
