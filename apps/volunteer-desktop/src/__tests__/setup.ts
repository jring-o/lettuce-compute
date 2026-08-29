import "@testing-library/jest-dom/vitest";

/**
 * Global test setup (vitest `setupFiles`).
 *
 * Tauri's `@tauri-apps/api/core` and `@tauri-apps/api/event` are aliased to
 * the mocks under `src/__mocks__/@tauri-apps/api/` by `vitest.config.ts`, so
 * every `invoke` resolves to a typed default (`hostCommandDefaults` in
 * `core.ts`) unless a test overrides it, and management-API calls can be
 * routed with `mockManagementApi`. This file only fills in the browser APIs
 * jsdom lacks.
 */

// IntersectionObserver (used by history page infinite scroll)
class MockIntersectionObserver {
  readonly root: Element | null = null;
  readonly rootMargin: string = "";
  readonly thresholds: ReadonlyArray<number> = [];
  constructor(
    _callback: IntersectionObserverCallback,
    _options?: IntersectionObserverInit
  ) {}
  observe() {}
  unobserve() {}
  disconnect() {}
  takeRecords(): IntersectionObserverEntry[] {
    return [];
  }
}
globalThis.IntersectionObserver =
  MockIntersectionObserver as unknown as typeof IntersectionObserver;

// window.matchMedia (used by settings page theme detection)
Object.defineProperty(window, "matchMedia", {
  writable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  }),
});
