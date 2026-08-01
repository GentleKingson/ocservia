import { readFile } from "node:fs/promises";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";
import { parse } from "yaml";

interface OpenApiDocument {
  openapi?: unknown;
  components?: {
    securitySchemes?: {
      oidc?: { type?: unknown; scheme?: unknown };
    };
    schemas?: {
      UuidV7?: { pattern?: unknown };
      Problem?: { required?: unknown };
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
    expect(document.components?.securitySchemes?.oidc).toMatchObject({
      type: "http",
      scheme: "bearer",
    });
  });
});
