import { expect, type Page } from "@playwright/test";

export interface ConsoleMockOptions {
  workspace: { id: string; name: string; slug: string; version?: number };
  nodes?: Record<string, unknown>[];
  recommendedAgentVersion?: string;
  default404?: boolean;
}

export function wireNode(
  id: string,
  name: string,
  overrides: Record<string, unknown> = {},
): Record<string, unknown> {
  return {
    id,
    name,
    version: 1,
    trust_status: "active",
    connection_state: "online",
    freshness: "fresh",
    dropped: { security: 0, health: 0, aggregate: 0, raw: 0 },
    session_count: 0,
    ...overrides,
  };
}

export async function expectNoPageOverflow(page: Page): Promise<void> {
  expect(
    await page.evaluate(
      () =>
        document.documentElement.scrollWidth <=
        document.documentElement.clientWidth,
    ),
  ).toBe(true);
}

export async function installEventSourceStub(
  page: Page,
  options?: { record?: boolean },
): Promise<void> {
  await page.addInitScript((record) => {
    const sources: Array<{
      url: string;
      closed: boolean;
      emitError: () => void;
    }> = [];
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
      private readonly entry?: {
        url: string;
        closed: boolean;
        emitError: () => void;
      };
      constructor(url: string | URL) {
        this.url = String(url);
        if (record) {
          this.entry = {
            url: this.url,
            closed: false,
            emitError: () => this.onerror?.(new Event("error")),
          };
          sources.push(this.entry);
        }
      }
      addEventListener(): void {}
      removeEventListener(): void {}
      dispatchEvent(): boolean {
        return true;
      }
      close(): void {
        if (this.entry) this.entry.closed = true;
      }
    }
    Object.defineProperty(window, "EventSource", { value: EventSourceStub });
    if (record) {
      Object.defineProperty(window, "__eventSources", { value: sources });
    }
  }, Boolean(options?.record));
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
