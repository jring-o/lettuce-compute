import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { invoke } from "@tauri-apps/api/core";

// Mock detectPlatform to return "windows" by default (jsdom's userAgent
// doesn't contain "Windows", so detectPlatform returns "linux" fallback
// which auto-completes the ContainerRuntimeStep).
const mockDetectPlatform = vi.fn(() => "windows" as const);

vi.mock("@/lib/utils", async () => {
  const actual = await vi.importActual<typeof import("@/lib/utils")>("@/lib/utils");
  return {
    ...actual,
    detectPlatform: () => mockDetectPlatform(),
  };
});

// invoke is auto-mocked via vitest alias
import { SetupWizard } from "./setup-wizard";

describe("SetupWizard", () => {
  let mockFetchOriginal: typeof globalThis.fetch;

  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    mockFetchOriginal = globalThis.fetch;
    mockDetectPlatform.mockReturnValue("windows");
  });

  afterEach(() => {
    globalThis.fetch = mockFetchOriginal;
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  /**
   * Return an invoke mock handler that returns "not_installed" for
   * get_container_runtime_status and valid prereqs for check_podman_prerequisites.
   */
  function makeNotInstalledInvokeMock() {
    return async (cmd: string) => {
      if (cmd === "get_container_runtime_status") {
        return {
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
        };
      }
      if (cmd === "check_podman_prerequisites") {
        return {
          wsl_available: true,
          podman_installed: false,
          podman_path: null,
          needs_install: true,
        };
      }
      return undefined;
    };
  }

  /**
   * Navigate from Welcome (step 0) through to Connect (step 5).
   * The ContainerRuntimeStep (step 4) calls getContainerRuntimeStatus() on mount.
   * We mock invoke to return "not_installed" status so the step shows a Skip button.
   */
  async function navigateToConnectStep(user: ReturnType<typeof userEvent.setup>) {
    // Step 0 -> 1 (Welcome -> Identity)
    await user.click(screen.getByText("Get Started"));
    // Step 1 -> 2 (Identity -> Resources)
    await user.click(screen.getByText("Next"));
    // Step 2 -> 3 (Resources -> Schedule)
    await user.click(screen.getByText("Next"));
    // Step 3 -> 4 (Schedule -> ContainerRuntime)
    await user.click(screen.getByText("Next"));

    // ContainerRuntimeStep mounts and calls getContainerRuntimeStatus().
    // On "windows" platform with not_installed status, shows Skip button.
    await waitFor(() => {
      expect(screen.getByText("Skip")).toBeInTheDocument();
    });

    // Step 4 -> 5 (ContainerRuntime -> Connect)
    await user.click(screen.getByText("Skip"));
  }

  describe("ConnectStep — head preview and leaf selection", () => {
    it("shows head preview after successful test connection", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      const mockFetch = vi.fn();

      vi.mocked(invoke).mockImplementation(makeNotInstalledInvokeMock());

      // Health check
      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ status: "healthy" }),
      });
      // Head info
      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: () =>
          Promise.resolve({
            name: "Science Server",
            description: "A compute server for science",
            leafs: [
              { slug: "prime", name: "Prime Study", research_area: "math", state: "ACTIVE" },
              { slug: "climate", name: "Climate Model", research_area: "earth", state: "ACTIVE" },
              { slug: "paused-leaf", name: "Paused Leaf", research_area: "bio", state: "PAUSED" },
            ],
          }),
      });
      vi.stubGlobal("fetch", mockFetch);

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToConnectStep(user);

      // Type URL and test connection
      const input = screen.getByPlaceholderText("https://compute.example.org");
      await user.type(input, "https://science.example.org");
      await user.click(screen.getByText("Test Connection"));

      // Head preview should appear
      await waitFor(() => {
        expect(screen.getByText("Science Server")).toBeInTheDocument();
      });
      expect(screen.getByText("A compute server for science")).toBeInTheDocument();

      // Only ACTIVE leafs shown
      expect(screen.getByText("Prime Study")).toBeInTheDocument();
      expect(screen.getByText("Climate Model")).toBeInTheDocument();
      expect(screen.queryByText("Paused Leaf")).not.toBeInTheDocument();
    });

    it("all active leafs are checked by default after test connection", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      const mockFetch = vi.fn();

      vi.mocked(invoke).mockImplementation(makeNotInstalledInvokeMock());

      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ status: "healthy" }),
      });
      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: () =>
          Promise.resolve({
            name: "Test Server",
            description: "",
            leafs: [
              { slug: "leaf-a", name: "Leaf A", research_area: "sci", state: "ACTIVE" },
              { slug: "leaf-b", name: "Leaf B", research_area: "sci", state: "ACTIVE" },
            ],
          }),
      });
      vi.stubGlobal("fetch", mockFetch);

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToConnectStep(user);

      const input = screen.getByPlaceholderText("https://compute.example.org");
      await user.type(input, "https://test.example.org");
      await user.click(screen.getByText("Test Connection"));

      await waitFor(() => {
        expect(screen.getByText("Leaf A")).toBeInTheDocument();
      });

      // All checkboxes should be checked by default
      const checkboxes = screen.getAllByRole("checkbox");
      for (const cb of checkboxes) {
        expect(cb).toBeChecked();
      }
    });

    it("toggling a leaf checkbox deselects it", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      const mockFetch = vi.fn();

      vi.mocked(invoke).mockImplementation(makeNotInstalledInvokeMock());

      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ status: "healthy" }),
      });
      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: () =>
          Promise.resolve({
            name: "Test Server",
            description: "",
            leafs: [
              { slug: "leaf-a", name: "Leaf A", research_area: "sci", state: "ACTIVE" },
              { slug: "leaf-b", name: "Leaf B", research_area: "sci", state: "ACTIVE" },
            ],
          }),
      });
      vi.stubGlobal("fetch", mockFetch);

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToConnectStep(user);

      const input = screen.getByPlaceholderText("https://compute.example.org");
      await user.type(input, "https://test.example.org");
      await user.click(screen.getByText("Test Connection"));

      await waitFor(() => {
        expect(screen.getByText("Leaf A")).toBeInTheDocument();
      });

      // Uncheck the first leaf
      const checkboxes = screen.getAllByRole("checkbox");
      await user.click(checkboxes[0]);

      expect(checkboxes[0]).not.toBeChecked();
      expect(checkboxes[1]).toBeChecked();
    });

    it("passes enabled_leafs to run_init when completing with leaf selection", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      const mockFetch = vi.fn();

      vi.mocked(invoke).mockImplementation(makeNotInstalledInvokeMock());

      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ status: "healthy" }),
      });
      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: () =>
          Promise.resolve({
            name: "Test Server",
            description: "",
            leafs: [
              { slug: "leaf-a", name: "Leaf A", research_area: "sci", state: "ACTIVE" },
              { slug: "leaf-b", name: "Leaf B", research_area: "sci", state: "ACTIVE" },
            ],
          }),
      });
      vi.stubGlobal("fetch", mockFetch);

      const onComplete = vi.fn();

      render(<SetupWizard onComplete={onComplete} />);
      await navigateToConnectStep(user);

      const input = screen.getByPlaceholderText("https://compute.example.org");
      await user.type(input, "https://test.example.org");
      await user.click(screen.getByText("Test Connection"));

      await waitFor(() => {
        expect(screen.getByText("Leaf A")).toBeInTheDocument();
      });

      // Uncheck leaf-a so only leaf-b is selected
      const checkboxes = screen.getAllByRole("checkbox");
      await user.click(checkboxes[0]);

      // Click "Start Contributing"
      await user.click(screen.getByText("Start Contributing"));

      await waitFor(() => {
        expect(invoke).toHaveBeenCalledWith("run_init", {
          config: expect.objectContaining({
            server_url: "https://test.example.org",
            enabled_leafs: ["leaf-b"],
          }),
        });
      });
    });

    it("passes enabled_leafs as null when completing without a server", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

      vi.mocked(invoke).mockImplementation(makeNotInstalledInvokeMock());

      const onComplete = vi.fn();

      render(<SetupWizard onComplete={onComplete} />);
      await navigateToConnectStep(user);

      // Click skip
      await user.click(screen.getByText(/Skip/));

      await waitFor(() => {
        expect(invoke).toHaveBeenCalledWith("run_init", {
          config: expect.objectContaining({
            server_url: null,
            enabled_leafs: null,
          }),
        });
      });
    });

    it("shows connection error on failed health check", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      const mockFetch = vi.fn();

      vi.mocked(invoke).mockImplementation(makeNotInstalledInvokeMock());

      mockFetch.mockRejectedValueOnce(new Error("Network error"));
      vi.stubGlobal("fetch", mockFetch);

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToConnectStep(user);

      const input = screen.getByPlaceholderText("https://compute.example.org");
      await user.type(input, "https://bad.example.com");
      await user.click(screen.getByText("Test Connection"));

      await waitFor(() => {
        expect(screen.getByText("Connection failed")).toBeInTheDocument();
      });
    });

    it("clears preview when URL changes", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      const mockFetch = vi.fn();

      vi.mocked(invoke).mockImplementation(makeNotInstalledInvokeMock());

      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ status: "healthy" }),
      });
      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: () =>
          Promise.resolve({
            name: "First Server",
            description: "",
            leafs: [],
          }),
      });
      vi.stubGlobal("fetch", mockFetch);

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToConnectStep(user);

      const input = screen.getByPlaceholderText("https://compute.example.org");
      await user.type(input, "https://first.example.org");
      await user.click(screen.getByText("Test Connection"));

      await waitFor(() => {
        expect(screen.getByText("First Server")).toBeInTheDocument();
      });

      // Changing the URL should clear the preview
      await user.clear(input);
      await user.type(input, "https://second.example.org");

      expect(screen.queryByText("First Server")).not.toBeInTheDocument();
    });

    it("shows error when run_init fails", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

      vi.mocked(invoke).mockImplementation(async (cmd: string) => {
        if (cmd === "get_container_runtime_status") {
          return {
            backend: "none", status: "not_installed", version: "",
            socket_path: "", machine_required: false, machine_name: "",
            machine_cpus: 0, machine_memory_mb: 0, machine_disk_gb: 0,
            error: null,
          };
        }
        if (cmd === "check_podman_prerequisites") {
          return { wsl_available: true, podman_installed: false, podman_path: null, needs_install: true };
        }
        if (cmd === "run_init") {
          throw new Error("Init failed: bad config");
        }
        return undefined;
      });

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToConnectStep(user);

      await user.click(screen.getByText("Start Contributing"));

      await waitFor(() => {
        expect(screen.getByText(/Init failed: bad config/)).toBeInTheDocument();
      });
    });

    it("prepends https:// when URL lacks protocol", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      const mockFetch = vi.fn();

      vi.mocked(invoke).mockImplementation(makeNotInstalledInvokeMock());

      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ status: "healthy" }),
      });
      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ name: "Server", description: "", leafs: [] }),
      });
      vi.stubGlobal("fetch", mockFetch);

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToConnectStep(user);

      const input = screen.getByPlaceholderText("https://compute.example.org");
      await user.type(input, "compute.example.org");
      await user.click(screen.getByText("Test Connection"));

      await waitFor(() => {
        expect(mockFetch).toHaveBeenCalledWith("https://compute.example.org/api/v1/health");
      });
    });
  });

  describe("ContainerRuntimeStep", () => {
    async function navigateToContainerStep(user: ReturnType<typeof userEvent.setup>) {
      // Step 0 -> 1 (Welcome -> Identity)
      await user.click(screen.getByText("Get Started"));
      // Step 1 -> 2 (Identity -> Resources)
      await user.click(screen.getByText("Next"));
      // Step 2 -> 3 (Resources -> Schedule)
      await user.click(screen.getByText("Next"));
      // Step 3 -> 4 (Schedule -> ContainerRuntime)
      await user.click(screen.getByText("Next"));
    }

    it("shows loading spinner while checking status", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

      // Never resolve the invoke so it stays in loading state
      vi.mocked(invoke).mockReturnValue(new Promise(() => {}));

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToContainerStep(user);

      expect(screen.getByText("Container Runtime")).toBeInTheDocument();
      expect(screen.getByText("Checking your system...")).toBeInTheDocument();
      // Skip button should be available even while loading
      expect(screen.getByText("Skip")).toBeInTheDocument();
    });

    it("shows success state when runtime is already running", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

      vi.mocked(invoke).mockImplementation(async (cmd: string) => {
        if (cmd === "get_container_runtime_status") {
          return {
            backend: "podman",
            status: "running",
            version: "5.3.1",
            socket_path: "/run/podman/podman.sock",
            machine_required: false,
            machine_name: "",
            machine_cpus: 0,
            machine_memory_mb: 0,
            machine_disk_gb: 0,
            error: null,
          };
        }
        return undefined;
      });

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToContainerStep(user);

      await waitFor(() => {
        expect(screen.getByText("Container runtime ready")).toBeInTheDocument();
      });
      expect(screen.getByText(/Podman/)).toBeInTheDocument();
      expect(screen.getByText(/v5\.3\.1/)).toBeInTheDocument();
    });

    it("shows Docker label when docker backend is running", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

      vi.mocked(invoke).mockImplementation(async (cmd: string) => {
        if (cmd === "get_container_runtime_status") {
          return {
            backend: "docker",
            status: "running",
            version: "24.0.7",
            socket_path: "/var/run/docker.sock",
            machine_required: false,
            machine_name: "",
            machine_cpus: 0,
            machine_memory_mb: 0,
            machine_disk_gb: 0,
            error: null,
          };
        }
        return undefined;
      });

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToContainerStep(user);

      await waitFor(() => {
        expect(screen.getByText("Container runtime ready")).toBeInTheDocument();
      });
      expect(screen.getByText(/Docker/)).toBeInTheDocument();
      expect(screen.getByText(/v24\.0\.7/)).toBeInTheDocument();
    });

    it("shows install guidance when runtime is not installed on Windows", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

      vi.mocked(invoke).mockImplementation(makeNotInstalledInvokeMock());

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToContainerStep(user);

      await waitFor(() => {
        // Windows prerequisites state shows Install & Set Up
        expect(screen.getByText("Install & Set Up")).toBeInTheDocument();
      });
      expect(screen.getByText("Skip")).toBeInTheDocument();
    });

    it("shows install guidance for macOS when not installed", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      mockDetectPlatform.mockReturnValue("macos");

      vi.mocked(invoke).mockImplementation(async (cmd: string) => {
        if (cmd === "get_container_runtime_status") {
          return {
            backend: "none", status: "not_installed", version: "",
            socket_path: "", machine_required: false, machine_name: "",
            machine_cpus: 0, machine_memory_mb: 0, machine_disk_gb: 0,
            error: null,
          };
        }
        return undefined;
      });

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToContainerStep(user);

      await waitFor(() => {
        expect(screen.getByText("Install Podman")).toBeInTheDocument();
      });
      expect(screen.getByText(/brew install podman/)).toBeInTheDocument();
    });

    it("shows setup button when runtime is not initialized", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

      vi.mocked(invoke).mockImplementation(async (cmd: string) => {
        if (cmd === "get_container_runtime_status") {
          return {
            backend: "podman",
            status: "not_initialized",
            version: "5.3.1",
            socket_path: "",
            machine_required: true,
            machine_name: "",
            machine_cpus: 0,
            machine_memory_mb: 0,
            machine_disk_gb: 0,
            error: null,
          };
        }
        if (cmd === "check_podman_prerequisites") {
          return { wsl_available: true, podman_installed: true, podman_path: null, needs_install: false };
        }
        return undefined;
      });

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToContainerStep(user);

      await waitFor(() => {
        expect(screen.getByText("Set Up")).toBeInTheDocument();
      });
    });

    it("calls setupContainerRuntime when setup button is clicked", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

      let setupCalled = false;
      vi.mocked(invoke).mockImplementation(async (cmd: string) => {
        if (cmd === "get_container_runtime_status") {
          if (setupCalled) {
            return {
              backend: "podman",
              status: "running",
              version: "5.3.1",
              socket_path: "/run/podman/podman.sock",
              machine_required: true,
              machine_name: "lettuce-vm",
              machine_cpus: 2,
              machine_memory_mb: 4096,
              machine_disk_gb: 10,
              error: null,
            };
          }
          return {
            backend: "podman",
            status: "not_initialized",
            version: "5.3.1",
            socket_path: "",
            machine_required: true,
            machine_name: "",
            machine_cpus: 0,
            machine_memory_mb: 0,
            machine_disk_gb: 0,
            error: null,
          };
        }
        if (cmd === "check_podman_prerequisites") {
          return { wsl_available: true, podman_installed: true, podman_path: null, needs_install: false };
        }
        if (cmd === "setup_container_runtime") {
          setupCalled = true;
          return { status: "ok", message: "Machine created" };
        }
        return undefined;
      });

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToContainerStep(user);

      await waitFor(() => {
        expect(screen.getByText("Set Up")).toBeInTheDocument();
      });

      await user.click(screen.getByText("Set Up"));

      // Should show progress stages while setting up
      await waitFor(() => {
        expect(screen.getByText("Setting Up Containers")).toBeInTheDocument();
      });

      // After polling completes, should show running state
      // Advance timers to trigger the 2-second poll interval
      await act(async () => {
        vi.advanceTimersByTime(2500);
      });

      await waitFor(() => {
        expect(screen.getByText("Container runtime ready")).toBeInTheDocument();
      });

      expect(invoke).toHaveBeenCalledWith("setup_container_runtime", expect.any(Object));
    });

    it("shows error when setup fails", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

      vi.mocked(invoke).mockImplementation(async (cmd: string) => {
        if (cmd === "get_container_runtime_status") {
          return {
            backend: "podman",
            status: "not_initialized",
            version: "5.3.1",
            socket_path: "",
            machine_required: true,
            machine_name: "",
            machine_cpus: 0,
            machine_memory_mb: 0,
            machine_disk_gb: 0,
            error: null,
          };
        }
        if (cmd === "check_podman_prerequisites") {
          return { wsl_available: true, podman_installed: true, podman_path: null, needs_install: false };
        }
        if (cmd === "setup_container_runtime") {
          throw new Error("WSL not available");
        }
        return undefined;
      });

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToContainerStep(user);

      await waitFor(() => {
        expect(screen.getByText("Set Up")).toBeInTheDocument();
      });

      await user.click(screen.getByText("Set Up"));

      await waitFor(() => {
        expect(screen.getByText(/WSL not available/)).toBeInTheDocument();
      });
    });

    it("Skip button advances past the container runtime step", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

      vi.mocked(invoke).mockImplementation(makeNotInstalledInvokeMock());

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToContainerStep(user);

      await waitFor(() => {
        expect(screen.getByText("Skip")).toBeInTheDocument();
      });

      await user.click(screen.getByText("Skip"));

      // Should now be on the Connect step (step 5)
      await waitFor(() => {
        expect(screen.getByText("Connect")).toBeInTheDocument();
        expect(screen.getByText("Add a server to start contributing compute.")).toBeInTheDocument();
      });
    });

    it("Back button returns to Schedule step", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

      vi.mocked(invoke).mockImplementation(makeNotInstalledInvokeMock());

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToContainerStep(user);

      await waitFor(() => {
        expect(screen.getByText("Container Runtime")).toBeInTheDocument();
      });

      await user.click(screen.getByText("Back"));

      // Should now be on the Schedule step (step 3)
      await waitFor(() => {
        expect(screen.getByText("Schedule")).toBeInTheDocument();
        expect(screen.getByText("When should Lettuce compute?")).toBeInTheDocument();
      });
    });

    it("shows 6 step indicators in the wizard", async () => {
      vi.mocked(invoke).mockResolvedValue(undefined);

      const { container } = render(<SetupWizard onComplete={vi.fn()} />);

      // The StepIndicator renders div elements with specific classes for each step
      const stepDots = container.querySelectorAll(".h-2.w-8.rounded-full");
      expect(stepDots.length).toBe(6);
    });

    it("hides navigation during setup progress", async () => {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });

      vi.mocked(invoke).mockImplementation(async (cmd: string) => {
        if (cmd === "get_container_runtime_status") {
          return {
            backend: "podman",
            status: "not_initialized",
            version: "5.3.1",
            socket_path: "",
            machine_required: true,
            machine_name: "",
            machine_cpus: 0,
            machine_memory_mb: 0,
            machine_disk_gb: 0,
            error: null,
          };
        }
        if (cmd === "check_podman_prerequisites") {
          return { wsl_available: true, podman_installed: true, podman_path: null, needs_install: false };
        }
        if (cmd === "setup_container_runtime") {
          // Never resolve — stays in "setting up" state
          return new Promise(() => {});
        }
        return undefined;
      });

      render(<SetupWizard onComplete={vi.fn()} />);
      await navigateToContainerStep(user);

      await waitFor(() => {
        expect(screen.getByText("Set Up")).toBeInTheDocument();
      });

      await user.click(screen.getByText("Set Up"));

      // While setting up, progress view is shown without navigation buttons
      await waitFor(() => {
        expect(screen.getByText("Setting Up Containers")).toBeInTheDocument();
      });

      // No Back or Skip buttons visible during install
      expect(screen.queryByText("Back")).not.toBeInTheDocument();
      expect(screen.queryByText("Skip")).not.toBeInTheDocument();
    });
  });
});
