import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { invoke } from "@tauri-apps/api/core";
import type { ContainerRuntimeDetection } from "@/api/client";

// detectPlatform reads navigator.userAgent, which under jsdom names neither
// Windows nor Mac, so the wizard would always take the Linux branches. Tests
// pick the platform explicitly.
const mockDetectPlatform = vi.fn<() => "windows" | "macos" | "linux">(() => "windows");

vi.mock("@/lib/utils", async () => {
  const actual = await vi.importActual<typeof import("@/lib/utils")>("@/lib/utils");
  return {
    ...actual,
    detectPlatform: () => mockDetectPlatform(),
  };
});

// invoke is auto-mocked via the vitest alias for @tauri-apps/api/core
import { SetupWizard } from "./setup-wizard";

type User = ReturnType<typeof userEvent.setup>;
type Handler = (cmd: string, args?: unknown) => unknown;

const NO_RUNTIME: ContainerRuntimeDetection = {
  backend: "none",
  version: "",
  binary_path: "",
  responding: false,
  detail: "",
};

const PODMAN_READY: ContainerRuntimeDetection = {
  backend: "podman",
  version: "5.3.1",
  binary_path: "/usr/bin/podman",
  responding: true,
  detail: "",
};

const HEAD = {
  name: "Science Server",
  description: "A compute server for science",
  leafs: [
    { slug: "prime", name: "Prime Study", research_area: ["math"], state: "ACTIVE" },
    { slug: "climate", name: "Climate Model", research_area: ["earth"], state: "ACTIVE" },
    { slug: "paused-leaf", name: "Paused Leaf", research_area: ["bio"], state: "PAUSED" },
  ],
};

/**
 * Route invoke calls by command name. Commands without a route resolve to
 * `undefined` (the mock's default, which also makes `run_init` succeed).
 */
function mockInvoke(routes: Record<string, Handler | unknown>) {
  vi.mocked(invoke).mockImplementation(async (cmd: string, args?: unknown) => {
    if (!(cmd in routes)) return undefined;
    const route = routes[cmd];
    return typeof route === "function" ? (route as Handler)(cmd, args) : route;
  });
}

/** A healthy head at any URL, with the leafs in `HEAD`. */
const headRoutes = {
  test_server_connection: { status: "healthy" },
  fetch_head_info: HEAD,
};

function runInitPayload(): Record<string, unknown> {
  const call = vi.mocked(invoke).mock.calls.find(([cmd]) => cmd === "run_init");
  expect(call, "run_init was not invoked").toBeDefined();
  return (call![1] as { config: Record<string, unknown> }).config;
}

/** Welcome -> Identity -> Resources -> Schedule (step 3). */
async function goToSchedule(user: User) {
  await user.click(screen.getByText("Get Started"));
  await user.click(screen.getByText("Next"));
  await user.click(screen.getByText("Next"));
  await screen.findByText("When should Lettuce compute?");
}

/** ... -> Container Runtime (step 4). */
async function goToContainerStep(user: User) {
  await goToSchedule(user);
  await user.click(screen.getByText("Next"));
  await screen.findByText("Container Runtime");
}

/**
 * ... -> Connect (step 5). With no engine detected the step shows its skip
 * button; with one detected it shows Next.
 */
async function goToConnect(user: User) {
  await goToContainerStep(user);
  const proceed = await screen.findByRole("button", {
    name: /Skip — WASM and native only|^Next$/,
  });
  await user.click(proceed);
  await screen.findByText("Add a server to start contributing compute.");
}

async function testConnection(user: User, url = "https://science.example.org") {
  await user.type(screen.getByPlaceholderText("compute.example.org"), url);
  await user.click(screen.getByText("Test Connection"));
  await screen.findByText("Connected");
}

describe("SetupWizard", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    mockDetectPlatform.mockReturnValue("windows");
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  function setup() {
    return userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
  }

  describe("IdentityStep", () => {
    it("explains that the keypair is the account and must be copied, never regenerated", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME });
      render(<SetupWizard onComplete={vi.fn()} />);
      await user.click(screen.getByText("Get Started"));

      expect(screen.getByText("The keypair is the account")).toBeInTheDocument();
      expect(screen.getAllByText("identity.key").length).toBeGreaterThan(0);
      expect(screen.getAllByText("identity.pub").length).toBeGreaterThan(0);
      expect(screen.getByText(/up to 10 machines per account/)).toBeInTheDocument();
      expect(screen.getByText(/before Lettuce starts there for the first time/)).toBeInTheDocument();
      expect(screen.getByText(/Never run setup again to "fix" a key/)).toBeInTheDocument();
    });

    it("names the data directory the host resolved as where the keys will be written", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, get_data_dir: "D:\profiles\second" });
      render(<SetupWizard onComplete={vi.fn()} />);
      await user.click(screen.getByText("Get Started"));

      await waitFor(() => {
        expect(screen.getByText("D:\profiles\second")).toBeInTheDocument();
      });
      expect(screen.queryByText("~/.lettuce")).not.toBeInTheDocument();
    });
  });

  describe("ScheduleStep", () => {
    it("offers always, when idle and scheduled windows", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToSchedule(user);

      expect(screen.getByRole("button", { name: /^Always/ })).toHaveAttribute("aria-pressed", "true");
      expect(screen.getByRole("button", { name: /^When idle/ })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /^Scheduled windows/ })).toBeInTheDocument();
      expect(screen.queryByText("CRON")).not.toBeInTheDocument();
    });

    it("shows the idle slider only for when idle", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToSchedule(user);

      expect(screen.queryByText(/idle time/)).not.toBeInTheDocument();
      await user.click(screen.getByRole("button", { name: /^When idle/ }));
      expect(screen.getByText("Start after this much idle time")).toBeInTheDocument();
      expect(screen.getByText("5 min")).toBeInTheDocument();
    });

    it("scheduled windows: hours, weekday checkboxes and a plain-language summary", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToSchedule(user);
      await user.click(screen.getByRole("button", { name: /^Scheduled windows/ }));

      expect(screen.getByLabelText("From")).toHaveValue("20");
      expect(screen.getByLabelText("To")).toHaveValue("6");
      for (const day of ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"]) {
        expect(screen.getByRole("checkbox", { name: day })).toBeChecked();
      }
      expect(
        screen.getByText("Lettuce will compute 20:00–06:00 (overnight) on every day.")
      ).toBeInTheDocument();

      await user.selectOptions(screen.getByLabelText("From"), "9");
      await user.selectOptions(screen.getByLabelText("To"), "17");
      await user.click(screen.getByRole("checkbox", { name: "Sat" }));
      await user.click(screen.getByRole("checkbox", { name: "Sun" }));
      expect(
        screen.getByText("Lettuce will compute 09:00–17:00 on Mon, Tue, Wed, Thu, Fri.")
      ).toBeInTheDocument();
    });

    it("refuses to continue with no days selected", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToSchedule(user);
      await user.click(screen.getByRole("button", { name: /^Scheduled windows/ }));

      for (const day of ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"]) {
        await user.click(screen.getByRole("checkbox", { name: day }));
      }
      expect(screen.getByText("Choose at least one day.")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Next" })).toBeDisabled();
    });
  });

  describe("run_init payload per schedule mode", () => {
    it("always: init --schedule-mode always, no threshold, no window", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await user.click(screen.getByText("Start Contributing"));

      await waitFor(() => expect(runInitPayload()).toMatchObject({
        schedule_mode: "always",
        idle_threshold_mins: null,
        schedule_window: null,
        server_url: null,
        trust: [],
        enabled_leafs: null,
      }));
    });

    it("when idle: init --schedule-mode idle with the threshold", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToSchedule(user);
      await user.click(screen.getByRole("button", { name: /^When idle/ }));
      await user.click(screen.getByText("Next"));
      await screen.findByText("Container Runtime");
      await user.click(await screen.findByRole("button", { name: "Skip — WASM and native only" }));
      await screen.findByText("Add a server to start contributing compute.");
      await user.click(screen.getByText("Start Contributing"));

      await waitFor(() => expect(runInitPayload()).toMatchObject({
        schedule_mode: "idle",
        idle_threshold_mins: 5,
        schedule_window: null,
      }));
    });

    it("scheduled: init runs as always and the window goes to schedule set", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToSchedule(user);
      await user.click(screen.getByRole("button", { name: /^Scheduled windows/ }));
      await user.selectOptions(screen.getByLabelText("From"), "19");
      await user.selectOptions(screen.getByLabelText("To"), "7");
      await user.click(screen.getByRole("checkbox", { name: "Sat" }));
      await user.click(screen.getByRole("checkbox", { name: "Sun" }));
      await user.click(screen.getByText("Next"));
      await screen.findByText("Container Runtime");
      await user.click(await screen.findByRole("button", { name: "Skip — WASM and native only" }));
      await screen.findByText("Add a server to start contributing compute.");
      await user.click(screen.getByText("Start Contributing"));

      await waitFor(() => expect(runInitPayload()).toMatchObject({
        schedule_mode: "always",
        idle_threshold_mins: null,
        schedule_window: {
          from_hour: 19,
          to_hour: 7,
          days: ["mon", "tue", "wed", "thu", "fri"],
        },
      }));
    });
  });

  describe("ConnectStep — head preview and leaf selection", () => {
    it("shows head preview after successful test connection", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user);

      // The head's name is also named in the trust consent wording below the preview.
      expect(screen.getByText("Science Server", { selector: "div" })).toBeInTheDocument();
      expect(screen.getByText("A compute server for science")).toBeInTheDocument();
      // Only ACTIVE leafs are offered; research areas are joined for display.
      expect(screen.getByText("Prime Study")).toBeInTheDocument();
      expect(screen.getByText("math")).toBeInTheDocument();
      expect(screen.getByText("Climate Model")).toBeInTheDocument();
      expect(screen.queryByText("Paused Leaf")).not.toBeInTheDocument();
      expect(invoke).toHaveBeenCalledWith("test_server_connection", { url: "science.example.org" });
      expect(invoke).toHaveBeenCalledWith("fetch_head_info", { url: "science.example.org" });
    });

    it("all active leafs are checked by default after test connection", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user);

      expect(screen.getByRole("checkbox", { name: "Prime Study" })).toBeChecked();
      expect(screen.getByRole("checkbox", { name: "Climate Model" })).toBeChecked();
    });

    it("toggling a leaf checkbox deselects it", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user);

      await user.click(screen.getByRole("checkbox", { name: "Prime Study" }));
      expect(screen.getByRole("checkbox", { name: "Prime Study" })).not.toBeChecked();
      expect(screen.getByRole("checkbox", { name: "Climate Model" })).toBeChecked();
    });

    it("passes enabled_leafs to run_init when completing with a partial leaf selection", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user, "https://test.example.org");

      await user.click(screen.getByRole("checkbox", { name: "Prime Study" }));
      await user.click(screen.getByText("Start Contributing"));

      await waitFor(() => expect(runInitPayload()).toMatchObject({
        server_url: "test.example.org",
        enabled_leafs: ["climate"],
      }));
    });

    it("refuses to start with a head whose every leaf is unchecked", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user);

      await user.click(screen.getByRole("checkbox", { name: "Prime Study" }));
      await user.click(screen.getByRole("checkbox", { name: "Climate Model" }));
      expect(screen.getByText(/Select at least one leaf/)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Start Contributing" })).toBeDisabled();
    });

    it("passes no server, no leafs and no trust when skipping", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, ...headRoutes });
      const onComplete = vi.fn();
      render(<SetupWizard onComplete={onComplete} />);
      await goToConnect(user);
      // Even with a tested head in the field, skipping attaches nothing.
      await testConnection(user);

      await user.click(screen.getByText("Skip — I'll add one later"));

      await waitFor(() => expect(runInitPayload()).toMatchObject({
        server_url: null,
        enabled_leafs: null,
        trust: [],
      }));
      await waitFor(() => expect(onComplete).toHaveBeenCalled());
    });

    it("requires a successful test before starting with a server", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);

      await user.type(screen.getByPlaceholderText("compute.example.org"), "https://x.example.org");
      expect(screen.getByRole("button", { name: "Start Contributing" })).toBeDisabled();
      expect(screen.getByText(/Test the connection/)).toBeInTheDocument();
      expect(screen.queryByText("What may this head run on your machine?")).not.toBeInTheDocument();
    });

    it("shows connection error on failed health check", async () => {
      const user = setup();
      mockInvoke({
        detect_container_runtime: NO_RUNTIME,
        test_server_connection: () => {
          throw new Error("Connection failed: dns error");
        },
      });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);

      await user.type(screen.getByPlaceholderText("compute.example.org"), "https://bad.example.com");
      await user.click(screen.getByText("Test Connection"));

      await screen.findByText("Connection failed");
      expect(invoke).not.toHaveBeenCalledWith("fetch_head_info", expect.anything());
    });

    it("clears preview and consent when URL changes", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user, "https://first.example.org");
      expect(screen.getByText("What may this head run on your machine?")).toBeInTheDocument();

      const input = screen.getByPlaceholderText("compute.example.org");
      await user.clear(input);
      await user.type(input, "https://second.example.org");

      expect(screen.queryByText("Science Server")).not.toBeInTheDocument();
      expect(screen.queryByText("What may this head run on your machine?")).not.toBeInTheDocument();
    });

    it("shows error when run_init fails", async () => {
      const user = setup();
      mockInvoke({
        detect_container_runtime: NO_RUNTIME,
        run_init: () => {
          throw new Error("Init failed: bad config");
        },
      });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);

      await user.click(screen.getByText("Start Contributing"));

      await screen.findByText(/Init failed: bad config/);
    });

    it("hands the bare host to the Rust command (it adds https:// itself)", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user, "compute.example.org");

      expect(invoke).toHaveBeenCalledWith("test_server_connection", { url: "compute.example.org" });
    });

    // TB-51: the old placeholder invited "https://…", the test passed with it,
    // and the CLI then stored the URL as a gRPC target it could never resolve.
    // The scheme and path are dropped before the probe, the field shows the
    // address the head will be stored under, and init receives that address.
    it("reduces a pasted URL to the head's address, shows it back and stores it", async () => {
      const user = setup();
      const onComplete = vi.fn();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, ...headRoutes });
      render(<SetupWizard onComplete={onComplete} />);
      await goToConnect(user);
      await testConnection(user, "https://Science.Example.org/");

      expect(invoke).toHaveBeenCalledWith("test_server_connection", { url: "science.example.org" });
      expect(invoke).toHaveBeenCalledWith("fetch_head_info", { url: "science.example.org" });
      expect(screen.getByPlaceholderText("compute.example.org")).toHaveValue("science.example.org");

      await user.click(screen.getByText("Start Contributing"));
      await waitFor(() => expect(onComplete).toHaveBeenCalled());
      expect(invoke).toHaveBeenCalledWith(
        "run_init",
        expect.objectContaining({ config: expect.objectContaining({ server_url: "science.example.org" }) })
      );
    });
  });

  describe("ConnectStep — runtime trust consent", () => {
    it("is shown after a successful test, with WASM always allowed", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user);

      expect(screen.getByText("What may this head run on your machine?")).toBeInTheDocument();
      expect(screen.getByText(/WASM tasks are always allowed \(sandboxed\)/)).toBeInTheDocument();
    });

    it("container is off and disabled when no engine was detected", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user);

      const container = screen.getByRole("checkbox", { name: "Allow container tasks from this head" });
      expect(container).not.toBeChecked();
      expect(container).toBeDisabled();
      expect(
        screen.getByText(/No Docker or Podman answered in the previous step, so container tasks cannot be offered/)
      ).toBeInTheDocument();
    });

    it("container defaults on when an engine was detected; native defaults off", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: PODMAN_READY, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user);

      const container = screen.getByRole("checkbox", { name: "Allow container tasks from this head" });
      expect(container).toBeChecked();
      expect(container).toBeEnabled();
      expect(screen.getByRole("checkbox", { name: "Allow native tasks from this head" })).not.toBeChecked();
      expect(screen.queryByText(/No Docker or Podman answered/)).not.toBeInTheDocument();
    });

    it("shows the native warning in plain language", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user);

      expect(
        screen.getByText(
          /runs a program directly on this machine with no sandbox\. It can read your files, including your identity key, and use your network\. Allow this only for an operator you fully trust\./
        )
      ).toBeInTheDocument();
    });

    it("sends trust: [container] by default with an engine detected", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: PODMAN_READY, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user);
      await user.click(screen.getByText("Start Contributing"));

      await waitFor(() => expect(runInitPayload()).toMatchObject({
        server_url: "science.example.org",
        trust: ["container"],
      }));
    });

    it("sends trust: [container, native] when native is allowed", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: PODMAN_READY, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user);
      await user.click(screen.getByRole("checkbox", { name: "Allow native tasks from this head" }));
      await user.click(screen.getByText("Start Contributing"));

      await waitFor(() => expect(runInitPayload()).toMatchObject({
        trust: ["container", "native"],
      }));
    });

    it("sends trust: [] (WASM only) when container is unchecked and no native", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: PODMAN_READY, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user);
      await user.click(screen.getByRole("checkbox", { name: "Allow container tasks from this head" }));
      await user.click(screen.getByText("Start Contributing"));

      await waitFor(() => expect(runInitPayload()).toMatchObject({
        server_url: "science.example.org",
        trust: [],
      }));
    });

    it("sends trust: [native] only, never container, when no engine was detected", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, ...headRoutes });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToConnect(user);
      await testConnection(user);
      await user.click(screen.getByRole("checkbox", { name: "Allow native tasks from this head" }));
      await user.click(screen.getByText("Start Contributing"));

      await waitFor(() => expect(runInitPayload()).toMatchObject({ trust: ["native"] }));
    });
  });

  describe("ContainerRuntimeStep", () => {
    it("shows a spinner and a skip button while checking", async () => {
      const user = setup();
      // Never resolves, so the step stays in its checking state.
      vi.mocked(invoke).mockReturnValue(new Promise(() => {}));
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToContainerStep(user);

      expect(screen.getByText("Checking your system...")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Skip — WASM and native only" })).toBeInTheDocument();
    });

    it("shows Ready with the Podman version when the engine answers", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: PODMAN_READY });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToContainerStep(user);

      await screen.findByText("Ready (Podman 5.3.1)");
      expect(invoke).not.toHaveBeenCalledWith("install_podman", expect.anything());
      expect(invoke).not.toHaveBeenCalledWith("get_container_runtime_status");
    });

    it("shows Ready with the Docker version when Docker answers", async () => {
      const user = setup();
      mockInvoke({
        detect_container_runtime: { ...PODMAN_READY, backend: "docker", version: "24.0.7" },
      });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToContainerStep(user);

      await screen.findByText("Ready (Docker 24.0.7)");
    });

    it("Windows, nothing installed: offers the bundled installer", async () => {
      const user = setup();
      mockInvoke({
        detect_container_runtime: NO_RUNTIME,
        check_podman_prerequisites: {
          wsl_available: true,
          podman_installed: false,
          podman_path: null,
          needs_install: true,
        },
      });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToContainerStep(user);

      await screen.findByText("Install & Set Up");
      expect(screen.getByText("No container runtime detected")).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Skip — WASM and native only" })).toBeInTheDocument();
    });

    it("Windows, WSL missing: explains how to enable it and offers skip", async () => {
      const user = setup();
      mockInvoke({
        detect_container_runtime: NO_RUNTIME,
        check_podman_prerequisites: {
          wsl_available: false,
          podman_installed: false,
          podman_path: null,
          needs_install: true,
        },
      });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToContainerStep(user);

      await screen.findByText("Enable WSL2");
      expect(screen.getByText(/wsl --install/)).toBeInTheDocument();
      expect(screen.queryByText("Install & Set Up")).not.toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Skip — WASM and native only" })).toBeInTheDocument();
    });

    it("Windows, Podman installed but no machine: offers Set Up and runs install_podman", async () => {
      const user = setup();
      let setUp = false;
      let finishInstall: (path: string) => void = () => {};
      mockInvoke({
        detect_container_runtime: () =>
          setUp
            ? PODMAN_READY
            : {
                ...PODMAN_READY,
                responding: false,
                detail: "Podman is installed but no Podman machine has been created.",
              },
        check_podman_prerequisites: {
          wsl_available: true,
          podman_installed: true,
          podman_path: "C:\\podman.exe",
          needs_install: false,
        },
        install_podman: () =>
          new Promise<string>((resolve) => {
            finishInstall = (path) => {
              setUp = true;
              resolve(path);
            };
          }),
      });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToContainerStep(user);

      await screen.findByText("Set Up");
      expect(
        screen.getByText("Podman is installed but no Podman machine has been created.")
      ).toBeInTheDocument();

      await user.click(screen.getByText("Set Up"));
      await screen.findByText("Setting Up Containers");
      // No navigation while the installer runs.
      expect(screen.queryByText("Back")).not.toBeInTheDocument();
      expect(screen.queryByText(/Skip/)).not.toBeInTheDocument();

      await act(async () => {
        finishInstall("C:\\podman.exe");
      });
      await screen.findByText("Ready (Podman 5.3.1)");
      expect(invoke).toHaveBeenCalledWith("install_podman", expect.any(Object));
    });

    it("shows the installer's error and offers Retry", async () => {
      const user = setup();
      mockInvoke({
        detect_container_runtime: NO_RUNTIME,
        check_podman_prerequisites: {
          wsl_available: true,
          podman_installed: false,
          podman_path: null,
          needs_install: true,
        },
        install_podman: () => {
          throw new Error("Podman installer failed (exit code 1603)");
        },
      });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToContainerStep(user);

      await user.click(await screen.findByText("Install & Set Up"));
      await screen.findByText(/Podman installer failed/);
      expect(screen.getByText("Retry")).toBeInTheDocument();
    });

    it("macOS, nothing installed: guidance only, never install_podman", async () => {
      const user = setup();
      mockDetectPlatform.mockReturnValue("macos");
      mockInvoke({ detect_container_runtime: NO_RUNTIME });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToContainerStep(user);

      await screen.findByText("No container runtime detected");
      expect(screen.getByText(/Podman Desktop/)).toBeInTheDocument();
      expect(screen.getByText(/Docker Desktop/)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Check again" })).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Skip — WASM and native only" })).toBeInTheDocument();
      expect(screen.queryByText("Install & Set Up")).not.toBeInTheDocument();
      expect(screen.queryByText("Set Up")).not.toBeInTheDocument();
      expect(invoke).not.toHaveBeenCalledWith("install_podman", expect.anything());
      expect(invoke).not.toHaveBeenCalledWith("check_podman_prerequisites");
    });

    it("macOS, Podman installed but machine stopped: says how to start it", async () => {
      const user = setup();
      mockDetectPlatform.mockReturnValue("macos");
      mockInvoke({
        detect_container_runtime: {
          ...PODMAN_READY,
          responding: false,
          detail: "Podman is installed but its machine is stopped.",
        },
      });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToContainerStep(user);

      await screen.findByText("Podman found (5.3.1)");
      expect(screen.getByText("Podman is installed but its machine is stopped.")).toBeInTheDocument();
      expect(screen.getByText("podman machine start")).toBeInTheDocument();
      expect(screen.queryByText("Set Up")).not.toBeInTheDocument();
    });

    it("Linux, nothing installed: distro packages and the user socket", async () => {
      const user = setup();
      mockDetectPlatform.mockReturnValue("linux");
      mockInvoke({ detect_container_runtime: NO_RUNTIME });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToContainerStep(user);

      await screen.findByText("No container runtime detected");
      expect(screen.getByText("sudo apt install podman")).toBeInTheDocument();
      expect(screen.getByText(/systemctl --user enable --now podman.socket/)).toBeInTheDocument();
      expect(screen.queryByText(/bundled/i)).not.toBeInTheDocument();
      expect(screen.queryByText("Set Up")).not.toBeInTheDocument();
      expect(invoke).not.toHaveBeenCalledWith("install_podman", expect.anything());
    });

    it("Linux, Podman installed but socket down: names the systemctl command", async () => {
      const user = setup();
      mockDetectPlatform.mockReturnValue("linux");
      mockInvoke({
        detect_container_runtime: {
          ...PODMAN_READY,
          responding: false,
          detail: "Podman is installed but its API socket is not running.",
        },
      });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToContainerStep(user);

      await screen.findByText("Podman is installed but its API socket is not running.");
      expect(screen.getByText("systemctl --user enable --now podman.socket")).toBeInTheDocument();
    });

    it("Check again re-runs detection and moves to Ready", async () => {
      const user = setup();
      mockDetectPlatform.mockReturnValue("macos");
      let calls = 0;
      mockInvoke({
        detect_container_runtime: () => (++calls >= 2 ? PODMAN_READY : NO_RUNTIME),
      });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToContainerStep(user);

      await user.click(await screen.findByRole("button", { name: "Check again" }));
      await screen.findByText("Ready (Podman 5.3.1)");
    });

    it("Skip advances to the Connect step", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToContainerStep(user);

      await user.click(await screen.findByRole("button", { name: "Skip — WASM and native only" }));
      await screen.findByText("Add a server to start contributing compute.");
    });

    it("Back returns to the Schedule step", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME });
      render(<SetupWizard onComplete={vi.fn()} />);
      await goToContainerStep(user);

      await user.click(screen.getByText("Back"));
      await screen.findByText("When should Lettuce compute?");
    });

    it("shows 6 step indicators in the wizard", () => {
      mockInvoke({ detect_container_runtime: NO_RUNTIME });
      const { container } = render(<SetupWizard onComplete={vi.fn()} />);
      const stepDots = container.querySelectorAll(".h-2.w-8.rounded-full");
      expect(stepDots.length).toBe(6);
    });
  });

  /**
   * TB-47: the CPU slider's maximum and the proposed default (half of it)
   * used to come from `navigator.hardwareConcurrency`, which WebKit (the Linux
   * and macOS web view) caps at 8 — so every such machine was set up with 4
   * cores, and the value is the hard CPU quota the daemon enforces. The Rust
   * host's count is the source now; the browser figure is only a fallback.
   */
  describe("core count comes from the host, not the web view (TB-47)", () => {
    beforeEach(() => {
      // What a WebKit web view reports on any machine with 8 or more threads.
      Object.defineProperty(navigator, "hardwareConcurrency", {
        value: 8,
        configurable: true,
      });
    });

    afterEach(() => {
      delete (navigator as { hardwareConcurrency?: number }).hardwareConcurrency;
    });

    it("sizes the CPU slider and proposes half of the host's cores", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, get_system_cpu_count: 32 });
      render(<SetupWizard onComplete={vi.fn()} />);
      await user.click(screen.getByText("Get Started"));
      await user.click(screen.getByText("Next"));

      await screen.findByText("16 / 32");
      // The first range input on the Resources step is the CPU slider.
      expect(screen.getAllByRole("slider")[0]).toHaveAttribute("max", "32");
      expect(screen.getAllByRole("slider")[0]).toHaveValue("16");
    });

    it("falls back to the web view's figure only when the host cannot report a count", async () => {
      const user = setup();
      mockInvoke({ detect_container_runtime: NO_RUNTIME, get_system_cpu_count: 0 });
      render(<SetupWizard onComplete={vi.fn()} />);
      await user.click(screen.getByText("Get Started"));
      await user.click(screen.getByText("Next"));

      await screen.findByText("4 / 8");
      expect(screen.getAllByRole("slider")[0]).toHaveAttribute("max", "8");
    });
  });

  describe("full run with an engine and a head", () => {
    it("sends every chosen value to run_init", async () => {
      const user = setup();
      mockInvoke({
        detect_container_runtime: PODMAN_READY,
        get_system_cpu_count: 32,
        ...headRoutes,
      });
      const onComplete = vi.fn();
      render(<SetupWizard onComplete={onComplete} />);
      await goToConnect(user);
      await testConnection(user);
      await user.click(screen.getByText("Start Contributing"));

      // cpu_cores is half of the host-reported count (TB-47), not of the
      // web view's capped figure.
      await waitFor(() => expect(runInitPayload()).toEqual({
        cpu_cores: 16,
        memory_mb: expect.any(Number),
        gpu_vram_pct: 50,
        disk_gb: 10,
        schedule_mode: "always",
        idle_threshold_mins: null,
        schedule_window: null,
        server_url: "science.example.org",
        trust: ["container"],
        enabled_leafs: null,
      }));
      await waitFor(() => expect(onComplete).toHaveBeenCalled());
    });
  });
});
