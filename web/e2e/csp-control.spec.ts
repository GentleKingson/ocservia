import { expect, test } from "@playwright/test";

type CspReporter = { __ocserviaCsp: (violation: string) => void };

test("gateway Report-Only CSP violations are observable in the page", async ({
  context,
  page,
}) => {
  test.skip(
    process.env.PLAYWRIGHT_GATEWAY !== "1",
    "needs the production gateway",
  );
  const violations: string[] = [];
  await context.exposeBinding("__ocserviaCsp", (_source, violation: string) => {
    violations.push(violation);
  });
  await context.addInitScript(() => {
    document.addEventListener("securitypolicyviolation", (event) => {
      (window as unknown as CspReporter).__ocserviaCsp(
        `${event.effectiveDirective}/${event.disposition}`,
      );
    });
  });
  const response = await page.goto("/");
  expect(
    response?.headers()["content-security-policy-report-only"],
  ).toBeTruthy();
  expect(response?.headers()["content-security-policy"]).toBeUndefined();
  await page.evaluate(() => {
    const image = new Image();
    image.src = "https://csp-control.invalid/pixel.png";
    document.body.append(image);
    const script = document.createElement("script");
    script.textContent = "document.body.dataset.cspControl = 'ran';";
    document.head.append(script);
  });
  await expect
    .poll(() => [...violations].sort())
    .toEqual(["img-src/report", "script-src-elem/report"]);
});
