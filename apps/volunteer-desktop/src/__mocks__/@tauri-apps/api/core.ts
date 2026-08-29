import { vi } from "vitest";

/**
 * Well-typed default results for every host-side (Rust) command the app
 * invokes, keyed by command name. A test that needs a specific value
 * overrides it (`invoke.mockImplementation`, or `mockManagementApi` with a
 * `fallback`). Keep this in step with `generate_handler![...]` in
 * `src-tauri/src/main.rs` and the `invoke(...)` calls under `src/`.
 *
 * `mgmt_request` is deliberately absent: it is routed by `mockManagementApi`
 * and otherwise resolves to `undefined`, which keeps
 * `ManagementClient.create()` succeeding without a routed status response.
 */
export const hostCommandDefaults: Record<string, unknown> = {
  // Lifecycle
  is_initialized: true,
  run_init: undefined,
  restart_daemon: undefined,
  get_data_dir: "/home/test/.lettuce",
  get_client_version: "0.0.0-test",
  // Host measurements
  system_metrics: { cpu_usage_pct: 0, memory_used_mb: 0, memory_total_mb: 0 },
  get_system_memory_mb: 16384,
  // Autostart and identity
  is_autostart_enabled: false,
  set_autostart: undefined,
  regenerate_keypair: "",
  // Updater
  install_update: undefined,
  // Container runtime
  get_container_runtime_status: {
    backend: "none",
    status: "not_installed",
    version: "",
    socket_path: "",
    machine_required: false,
    machine_name: "",
    machine_cpus: 0,
    machine_memory_mb: 0,
    machine_disk_gb: 0,
    error: null,
  },
  setup_container_runtime: { status: "ok", message: "" },
  start_container_runtime: { status: "ok", message: "" },
  stop_container_runtime: { status: "ok", message: "" },
  check_podman_prerequisites: {
    wsl_available: true,
    podman_installed: false,
    podman_path: null,
    needs_install: true,
  },
  install_podman: "",
  // Heads reached before the daemon exists
  test_server_connection: true,
  fetch_head_info: { name: "", description: "", leafs: [] },
  // Visualization bridge
  set_viz_base: undefined,
  viz_read_file: [],
  viz_list_files: [],
};

/** The default result for a command: its entry in `hostCommandDefaults`, else `undefined`. */
export function defaultCommandResult(cmd: string): unknown {
  return cmd in hostCommandDefaults ? hostCommandDefaults[cmd] : undefined;
}

/**
 * Mock of Tauri's `invoke`. By default every command resolves to its entry in
 * `hostCommandDefaults` (`undefined` for anything unlisted, `mgmt_request`
 * included), which also makes `ManagementClient.create()` succeed: its probe
 * of `GET /api/v1/status` is just another `mgmt_request` invoke.
 */
export const invoke = vi.fn((cmd: string, _args?: unknown): Promise<unknown> =>
  Promise.resolve(defaultCommandResult(cmd))
);

/**
 * Mock of Tauri's `convertFileSrc`: the Windows/Android URL form of a custom
 * protocol (`http://<protocol>.localhost/<path>`). On macOS and Linux the real
 * one yields `<protocol>://localhost/<path>`; tests should assert on the
 * protocol name and path, not the exact form.
 */
export const convertFileSrc = vi.fn(
  (filePath: string, protocol: string = "asset"): string =>
    `http://${protocol}.localhost/${encodeURIComponent(filePath)}`
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
 * query string) for tests of daemon-backed code. Other commands resolve to
 * `fallback(cmd, args)` when given, else to their `hostCommandDefaults`
 * entry. Unrouted management-API calls reject with a `NOT_FOUND` error so a
 * test cannot pass by accident.
 */
export function mockManagementApi(
  routes: Record<string, MgmtRoute>,
  fallback?: (cmd: string, args?: unknown) => unknown
): void {
  invoke.mockImplementation(async (cmd: string, args?: unknown) => {
    if (cmd !== "mgmt_request") {
      return fallback ? fallback(cmd, args) : defaultCommandResult(cmd);
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
