import { expect, test } from "@playwright/test";

type CspReporter = { __ocserviaCsp: (violation: string) => void };

test("gateway CSP blocks violations observably in the page", async ({
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
  expect(response?.headers()["content-security-policy"]).toBeTruthy();
  expect(
    response?.headers()["content-security-policy-report-only"],
  ).toBeUndefined();
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
    .toEqual(["img-src/enforce", "script-src-elem/enforce"]);
  expect(
    await page.evaluate(() => document.body.dataset.cspControl),
  ).toBeUndefined();
});
