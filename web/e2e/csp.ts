import { expect, test as base } from "@playwright/test";

export { expect };

type CspReporter = { __ocserviaCsp: (violation: string) => void };

// The gateway CSP is enforced and has no report endpoint, so every test
// collects violations in its own pages and fails on any. Without the gateway
// (Vite preview) no policy is sent; the control spec proves collection there.
export const test = base.extend<{ cspViolations: string[] }>({
  cspViolations: [
    async ({ context }, use) => {
      const violations: string[] = [];
      await context.exposeBinding(
        "__ocserviaCsp",
        (_source, violation: string) => {
          violations.push(violation);
        },
      );
      await context.addInitScript(() => {
        document.addEventListener("securitypolicyviolation", (event) => {
          (window as unknown as CspReporter).__ocserviaCsp(
            `${event.effectiveDirective} ${event.blockedURI}`,
          );
        });
      });
      await use(violations);
      expect(violations, "Content Security Policy violations").toEqual([]);
    },
    { auto: true },
  ],
});
