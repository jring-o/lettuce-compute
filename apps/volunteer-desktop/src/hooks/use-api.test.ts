import { describe, it, expect, vi, beforeEach } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import type { ManagementClient } from "@/api/client";

/**
 * `use-api` keeps the connected client in module state, so every test gets a
 * fresh copy of the module (and of the aliased Tauri mock it imports).
 */
async function freshModules() {
  vi.resetModules();
  const core = await import("@tauri-apps/api/core");
  const api = await import("./use-api");
  return { invoke: vi.mocked(core.invoke), useApiQuery: api.useApiQuery };
}

const STATUS = {
  state: "active",
  uptime_seconds: 5,
  connected_servers: 1,
  active_tasks: [],
  queued_tasks: [],
  failing_leafs: [],
  paused_reason: null,
};

describe("useApiQuery while the daemon is starting", () => {
  beforeEach(() => {
    vi.useRealTimers();
  });

  // TB-55: the layout's status bar mounts at app launch, before the daemon
  // answers. The first connection attempt fails, the retry 2 s later
  // succeeds — and the query mounted during the failure must then run and
  // poll. It used to keep the first attempt's error as terminal and show
  // "Stopped" until the window was reloaded.
  it("polls once the connection retry succeeds", async () => {
    const { invoke, useApiQuery } = await freshModules();
    let statusCalls = 0;
    invoke.mockImplementation(async (cmd: string) => {
      if (cmd !== "mgmt_request") return undefined;
      statusCalls += 1;
      if (statusCalls === 1) {
        throw {
          code: "DAEMON_UNREACHABLE",
          message: "management API request failed: connection refused",
          status: 0,
        };
      }
      return STATUS;
    });

    const early = renderHook(() =>
      useApiQuery((client: ManagementClient) => client.status(), 300)
    );
    await waitFor(() => expect(early.result.current.error).not.toBeNull());
    expect(early.result.current.data).toBeNull();

    // The retry fires after 2 s and succeeds.
    await waitFor(() => expect(early.result.current.data).not.toBeNull(), {
      timeout: 6000,
    });
    expect(early.result.current.error).toBeNull();
    expect(early.result.current.data).toMatchObject({ state: "active" });
  }, 10000);

  it("reports the connection error while there is no client", async () => {
    const { invoke, useApiQuery } = await freshModules();
    invoke.mockImplementation(async (cmd: string) => {
      if (cmd !== "mgmt_request") return undefined;
      throw { code: "DAEMON_UNREACHABLE", message: "no daemon.json", status: 0 };
    });

    const { result } = renderHook(() =>
      useApiQuery((client: ManagementClient) => client.status())
    );
    await waitFor(() => expect(result.current.error).not.toBeNull());
    expect(result.current.isLoading).toBe(false);
    expect(result.current.data).toBeNull();
  });
});
