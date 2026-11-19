import { createRenderer, ssrContextKey } from "vue";

export const renderer = createRenderer<object, object>({
  createElement: () => ({}),
  createText: () => ({}),
  createComment: () => ({}),
  insert: () => {},
  remove: () => {},
  setText: () => {},
  setElementText: () => {},
  parentNode: () => null,
  nextSibling: () => null,
  patchProp: () => {},
});

export function mountSetup(
  component: object,
  props?: Record<string, unknown>,
): { view: unknown; unmount: () => void } {
  const app = renderer.createApp({ ...component, render: () => null }, props);
  app.provide(ssrContextKey, {});
  const instance = app.mount({});
  let mounted = true;
  return {
    view: (instance.$ as unknown as { setupState: unknown }).setupState,
    unmount: () => {
      if (!mounted) return;
      mounted = false;
      app.unmount();
    },
  };
}
