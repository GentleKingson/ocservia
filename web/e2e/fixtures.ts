import type { Page } from "@playwright/test";

export interface ConsoleMockOptions {
  workspace: { id: string; name: string; slug: string; version?: number };
  nodes?: Record<string, unknown>[];
  recommendedAgentVersion?: string;
  default404?: boolean;
}

export async function installEventSourceStub(page: Page): Promise<void> {
  await page.addInitScript(() => {
    class EventSourceStub {
      static readonly CONNECTING = 0;
      static readonly OPEN = 1;
      static readonly CLOSED = 2;
      readonly CONNECTING = 0;
      readonly OPEN = 1;
      readonly CLOSED = 2;
      readonly withCredentials = false;
      readonly readyState = 1;
      readonly url: string;
      onerror: ((event: Event) => void) | null = null;
      onmessage: ((event: MessageEvent) => void) | null = null;
      onopen: ((event: Event) => void) | null = null;
      constructor(url: string | URL) {
        this.url = String(url);
      }
      addEventListener(): void {}
      removeEventListener(): void {}
      dispatchEvent(): boolean {
        return true;
      }
      close(): void {}
    }
    Object.defineProperty(window, "EventSource", { value: EventSourceStub });
  });
}

export async function installConsoleMocks(
  page: Page,
  options: ConsoleMockOptions,
): Promise<void> {
  await installEventSourceStub(page);
  if (options.default404) {
    await page.route("**/api/v1/**", (route) =>
      route.fulfill({ status: 404, json: {} }),
    );
  }
  await page.route("**/api/v1/readyz", (route) => route.fulfill({ json: {} }));
  if (options.recommendedAgentVersion !== undefined) {
    await page.route("**/api/v1/version", (route) =>
      route.fulfill({
        json: {
          version: "dev",
          commit: "unknown",
          role: "all",
          recommended_agent_version: options.recommendedAgentVersion,
        },
      }),
    );
  }
  await page.route("**/api/v1/workspaces", (route) =>
    route.fulfill({
      json: {
        items: [{ version: 1, ...options.workspace }],
      },
    }),
  );
  await page.route("**/api/v1/nodes?**", (route) =>
    route.fulfill({
      json: { items: options.nodes ?? [], page: { has_more: false } },
    }),
  );
}
