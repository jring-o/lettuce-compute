import { vi } from "vitest";

/**
 * Mock of Tauri's `invoke`. By default every command resolves to `undefined`,
 * which also makes `ManagementClient.create()` succeed (its probe of
 * `GET /api/v1/status` is just another `mgmt_request` invoke).
 */
export const invoke = vi.fn((_cmd: string, _args?: unknown): Promise<unknown> =>
  Promise.resolve(undefined)
);

/** The arguments the client passes to the `mgmt_request` command. */
export interface MgmtRequestArgs {
  method: string;
  path: string;
  body: unknown;
}

/**
 * A response for one management-API route, or a function computing it from
 * the request. Throw (or return a rejected promise) to simulate a daemon
 * error; the rejection value is what the Rust command would produce, i.e.
 * `{ code, message, status }`.
 */
export type MgmtRoute = unknown | ((args: MgmtRequestArgs) => unknown);

/**
 * Route `mgmt_request` invokes by `"METHOD /path"` (the path without its
 * query string) for tests of daemon-backed code. Other commands keep resolving
 * to `undefined` unless `fallback` says otherwise. Unrouted management-API
 * calls reject with a `NOT_FOUND` error so a test cannot pass by accident.
 */
export function mockManagementApi(
  routes: Record<string, MgmtRoute>,
  fallback?: (cmd: string, args?: unknown) => unknown
): void {
  invoke.mockImplementation(async (cmd: string, args?: unknown) => {
    if (cmd !== "mgmt_request") {
      return fallback ? fallback(cmd, args) : undefined;
    }
    const req = args as MgmtRequestArgs;
    const pathOnly = req.path.split("?")[0];
    const key = `${req.method} ${pathOnly}`;
    if (!(key in routes)) {
      throw { code: "NOT_FOUND", message: `no mock route for ${key}`, status: 404 };
    }
    const route = routes[key];
    return typeof route === "function"
      ? (route as (args: MgmtRequestArgs) => unknown)(req)
      : route;
  });
}
