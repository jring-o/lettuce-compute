import { useState, useEffect, useCallback, type ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Slider } from "@/components/ui/slider";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import { cn, detectPlatform } from "@/lib/utils";
import {
  buildInitSchedule,
  describeWindow,
  formatHour,
  WEEKDAYS,
  WEEKDAY_LABELS,
  type Weekday,
  type WizardScheduleMode,
} from "@/lib/schedule-mode";
import lettuceLeaf from "@/assets/lettuce-leaf.png";
import { RuntimeTrustFields } from "@/components/heads/runtime-trust-fields";
import type { HeadPreview, ContainerRuntimeDetection, PodmanPrerequisites } from "@/api/client";
import {
  checkPodmanPrerequisites,
  detectContainerRuntime,
  fetchHeadInfo,
  getDataDir,
  getSystemMemoryMb,
  installPodman,
  runInit,
  testServerConnection,
} from "@/api/client";

interface WizardProps {
  onComplete: () => void;
}

interface WizardState {
  cpuCores: number;
  memoryMb: number;
  gpuVramPct: number;
  diskGb: number;
  totalMemoryMb: number;
  scheduleMode: WizardScheduleMode;
  idleThresholdMins: number;
  scheduleFromHour: number;
  scheduleToHour: number;
  scheduleDays: Weekday[];
  /** A container engine answered in the Container Runtime step. */
  containerRuntimeDetected: boolean;
  serverUrl: string;
  /** "Test Connection" succeeded for the current `serverUrl`. */
  connectionOk: boolean;
  headPreview: HeadPreview | null;
  enabledLeafSlugs: string[];
  /** Runtime-trust consent for the head at `serverUrl` (WASM is always allowed). */
  trustContainer: boolean;
  trustNative: boolean;
}

const maxCpuCores = navigator.hardwareConcurrency || 4;
// Fallback when OS memory detection fails. Real total RAM is detected on mount
// (get_system_memory_mb) and stored in wizard state — the old hardcoded 8 GB
// capped the memory slider at ~7.4 GB, below the floor of large-memory leaves
// (e.g. extract2 needs ≥28 GB), so those volunteers were never matched to work.
const FALLBACK_TOTAL_MEMORY_MB = 8192;

const SKIP_CONTAINER_LABEL = "Skip — WASM and native only";

/**
 * Why the Connect step cannot offer container trust: the Container Runtime
 * step just probed this machine and no engine answered (or the step was
 * skipped). Trust can be widened later from the Projects page.
 */
const WIZARD_CONTAINER_UNAVAILABLE_NOTE =
  "No Docker or Podman answered in the previous step, so container tasks cannot be offered. Once an engine is running you can allow them from the Projects page.";

const HOURS = Array.from({ length: 24 }, (_, h) => h);

function StepIndicator({ current, total }: { current: number; total: number }) {
  return (
    <div className="flex items-center justify-center gap-2 mb-8">
      {Array.from({ length: total }, (_, i) => (
        <div
          key={i}
          className={cn(
            "h-2 w-8 rounded-full transition-colors",
            i < current ? "bg-primary" : i === current ? "bg-primary/60" : "bg-muted"
          )}
        />
      ))}
    </div>
  );
}

function WelcomeStep({ onNext }: { onNext: () => void }) {
  return (
    <div className="flex flex-col items-center text-center space-y-6">
      <img src={lettuceLeaf} alt="Lettuce" className="w-64 rounded-lg" />
      <h1 className="text-3xl font-bold">Welcome to Lettuce Compute</h1>
      <p className="text-muted-foreground max-w-md">
        You're about to start contributing your idle compute power to science.
        Let's set up your volunteer identity and preferences.
      </p>
      <Button size="lg" onClick={onNext}>
        Get Started
      </Button>
    </div>
  );
}

function IdentityStep({
  dataDir,
  onNext,
  onBack,
}: {
  /** Where the keys will be written: the app's data directory. */
  dataDir: string;
  onNext: () => void;
  onBack: () => void;
}) {
  return (
    <div className="space-y-6 max-w-md mx-auto">
      <div className="text-center space-y-2">
        <h2 className="text-2xl font-bold">Identity</h2>
        <p className="text-muted-foreground">
          Your account is a keypair on this computer. Setup creates it for you.
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">The keypair is the account</CardTitle>
          <CardDescription>
            When setup completes, Lettuce generates two files in{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-xs">{dataDir}</code>:{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-xs">identity.key</code> (private)
            and <code className="rounded bg-muted px-1 py-0.5 text-xs">identity.pub</code>. There
            is no username or password — those two files are your account, and credit for
            completed work is recorded against them.
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-3 text-sm text-muted-foreground">
          <p>
            <span className="font-medium text-foreground">Several machines, one account.</span>{" "}
            A head tracks up to 10 machines per account separately (its default); more still run,
            but share one work allowance. To use this account on another machine, copy{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-xs">identity.key</code> and{" "}
            <code className="rounded bg-muted px-1 py-0.5 text-xs">identity.pub</code> into that
            machine's data directory (the same folder, unless it was moved) before Lettuce starts
            there for the first time.
          </p>
          <p>
            <span className="font-medium text-foreground">Never run setup again to "fix" a key.</span>{" "}
            Setup would generate a new keypair, which is a new account, and the credit on the old
            one stays behind. If a key will not load, restore it from your copy instead.
          </p>
        </CardContent>
      </Card>

      <div className="flex justify-between">
        <Button variant="ghost" onClick={onBack}>
          Back
        </Button>
        <Button onClick={onNext}>Next</Button>
      </div>
    </div>
  );
}

function ResourcesStep({
  state,
  onChange,
  onNext,
  onBack,
}: {
  state: WizardState;
  onChange: (s: Partial<WizardState>) => void;
  onNext: () => void;
  onBack: () => void;
}) {
  return (
    <div className="space-y-6 max-w-md mx-auto">
      <div className="text-center space-y-2">
        <h2 className="text-2xl font-bold">Resources</h2>
        <p className="text-muted-foreground">
          Choose how much of your computer to share.
        </p>
      </div>

      <div className="space-y-5">
        <div className="space-y-2">
          <div className="flex justify-between text-sm">
            <span>CPU Cores</span>
            <span className="font-medium">
              {state.cpuCores} / {maxCpuCores}
            </span>
          </div>
          <Slider
            min={1}
            max={maxCpuCores}
            value={state.cpuCores}
            onChange={(v) => onChange({ cpuCores: v })}
          />
        </div>

        <div className="space-y-2">
          <div className="flex justify-between text-sm">
            <span>Memory</span>
            <span className="font-medium">{state.memoryMb} MB</span>
          </div>
          <Slider
            min={256}
            max={Math.max(256, Math.round(state.totalMemoryMb * 0.9))}
            step={256}
            value={state.memoryMb}
            onChange={(v) => onChange({ memoryMb: v })}
          />
        </div>

        <div className="space-y-2">
          <div className="flex justify-between text-sm">
            <span>GPU VRAM</span>
            <span className="font-medium">
              {state.gpuVramPct === 0 ? "Disabled" : `${state.gpuVramPct}%`}
            </span>
          </div>
          <Slider
            min={0}
            max={100}
            step={10}
            value={state.gpuVramPct}
            onChange={(v) => onChange({ gpuVramPct: v })}
          />
        </div>

        <div className="space-y-2">
          <div className="flex justify-between text-sm">
            <span>Disk Storage</span>
            <span className="font-medium">{state.diskGb} GB</span>
          </div>
          <Slider
            min={1}
            max={500}
            value={state.diskGb}
            onChange={(v) => onChange({ diskGb: v })}
          />
        </div>
      </div>

      <div className="flex justify-between">
        <Button variant="ghost" onClick={onBack}>
          Back
        </Button>
        <Button onClick={onNext}>Next</Button>
      </div>
    </div>
  );
}

function HourSelect({
  id,
  label,
  value,
  onChange,
}: {
  id: string;
  label: string;
  value: number;
  onChange: (hour: number) => void;
}) {
  return (
    <label htmlFor={id} className="flex flex-col gap-1 text-sm">
      <span>{label}</span>
      <select
        id={id}
        value={value}
        onChange={(e) => onChange(Number(e.target.value))}
        className="h-10 rounded-md border border-input bg-background px-3 text-sm"
      >
        {HOURS.map((h) => (
          <option key={h} value={h}>
            {formatHour(h)}
          </option>
        ))}
      </select>
    </label>
  );
}

function ScheduleStep({
  state,
  onChange,
  onNext,
  onBack,
}: {
  state: WizardState;
  onChange: (s: Partial<WizardState>) => void;
  onNext: () => void;
  onBack: () => void;
}) {
  const modes: { value: WizardScheduleMode; label: string; desc: string }[] = [
    { value: "always", label: "Always", desc: "Compute whenever Lettuce is running." },
    {
      value: "idle",
      label: "When idle",
      desc: "Only after you have not used this computer for a while.",
    },
    {
      value: "scheduled",
      label: "Scheduled windows",
      desc: "Only inside a daily time window, on the days you choose.",
    },
  ];

  const window = {
    from_hour: state.scheduleFromHour,
    to_hour: state.scheduleToHour,
    days: state.scheduleDays,
  };
  const noDays = state.scheduleMode === "scheduled" && state.scheduleDays.length === 0;

  const toggleDay = (day: Weekday) => {
    const days = state.scheduleDays.includes(day)
      ? state.scheduleDays.filter((d) => d !== day)
      : [...state.scheduleDays, day];
    onChange({ scheduleDays: days });
  };

  return (
    <div className="space-y-6 max-w-md mx-auto">
      <div className="text-center space-y-2">
        <h2 className="text-2xl font-bold">Schedule</h2>
        <p className="text-muted-foreground">When should Lettuce compute?</p>
      </div>

      <div className="grid gap-3">
        {modes.map((mode) => (
          <button
            key={mode.value}
            type="button"
            aria-pressed={state.scheduleMode === mode.value}
            onClick={() => onChange({ scheduleMode: mode.value })}
            className={cn(
              "rounded-lg border p-4 text-left transition-colors",
              state.scheduleMode === mode.value
                ? "border-primary bg-primary/5"
                : "hover:bg-muted/50"
            )}
          >
            <div className="font-medium">{mode.label}</div>
            <div className="text-sm text-muted-foreground">{mode.desc}</div>
          </button>
        ))}
      </div>

      {state.scheduleMode === "idle" && (
        <div className="space-y-2">
          <div className="flex justify-between text-sm">
            <span>Start after this much idle time</span>
            <span className="font-medium">{state.idleThresholdMins} min</span>
          </div>
          <Slider
            min={1}
            max={60}
            value={state.idleThresholdMins}
            onChange={(v) => onChange({ idleThresholdMins: v })}
          />
        </div>
      )}

      {state.scheduleMode === "scheduled" && (
        <div className="space-y-4">
          <div className="grid grid-cols-2 gap-3">
            <HourSelect
              id="schedule-from"
              label="From"
              value={state.scheduleFromHour}
              onChange={(h) => onChange({ scheduleFromHour: h })}
            />
            <HourSelect
              id="schedule-to"
              label="To"
              value={state.scheduleToHour}
              onChange={(h) => onChange({ scheduleToHour: h })}
            />
          </div>
          <p className="text-xs text-muted-foreground">
            Whole hours only. A window may run past midnight, so 20:00 to 06:00 is
            "overnight". The same hour for both means the whole day.
          </p>
          <div className="space-y-2">
            <span className="text-sm">On these days</span>
            <div className="flex flex-wrap gap-2">
              {WEEKDAYS.map((day) => (
                <label
                  key={day}
                  className={cn(
                    "flex cursor-pointer items-center gap-1.5 rounded-md border px-2.5 py-1.5 text-sm",
                    state.scheduleDays.includes(day) ? "border-primary bg-primary/5" : ""
                  )}
                >
                  <input
                    type="checkbox"
                    aria-label={WEEKDAY_LABELS[day]}
                    checked={state.scheduleDays.includes(day)}
                    onChange={() => toggleDay(day)}
                    className="h-4 w-4 rounded border-input accent-primary"
                  />
                  {WEEKDAY_LABELS[day]}
                </label>
              ))}
            </div>
          </div>
          <div className="rounded-md bg-muted p-3 text-sm">
            {noDays ? "Choose at least one day." : `Lettuce will compute ${describeWindow(window)}.`}
          </div>
        </div>
      )}

      <div className="flex justify-between">
        <Button variant="ghost" onClick={onBack}>
          Back
        </Button>
        <Button onClick={onNext} disabled={noDays}>
          Next
        </Button>
      </div>
    </div>
  );
}

type RuntimeStage =
  | "checking"
  | "ready"
  | "guidance"
  | "wsl_required"
  | "installing"
  | "initializing"
  | "error";

function backendLabel(backend: ContainerRuntimeDetection["backend"]): string {
  return backend === "docker" ? "Docker" : "Podman";
}

function ContainerRuntimeStep({
  state,
  onDetected,
  onNext,
  onBack,
}: {
  state: WizardState;
  onDetected: (detected: boolean) => void;
  onNext: () => void;
  onBack: () => void;
}) {
  const [detection, setDetection] = useState<ContainerRuntimeDetection | null>(null);
  const [prereqs, setPrereqs] = useState<PodmanPrerequisites | null>(null);
  const [stage, setStage] = useState<RuntimeStage>("checking");
  const [installError, setInstallError] = useState<string | null>(null);
  const [autoAdvanced, setAutoAdvanced] = useState(false);

  const platform = detectPlatform();

  // Probe the machine directly (`detect_container_runtime`); the daemon does
  // not exist yet during first-run setup, so its status API cannot be asked.
  const check = useCallback(async () => {
    setStage("checking");
    setInstallError(null);
    let found: ContainerRuntimeDetection;
    try {
      found = await detectContainerRuntime();
    } catch (err) {
      setDetection(null);
      onDetected(false);
      setInstallError(String(err));
      setStage("error");
      return;
    }
    setDetection(found);
    onDetected(found.responding);
    if (found.responding) {
      setStage("ready");
      return;
    }
    if (platform === "windows") {
      // The bundled installer needs WSL2; find out before offering it.
      try {
        const p = await checkPodmanPrerequisites();
        setPrereqs(p);
        if (!p.wsl_available) {
          setStage("wsl_required");
          return;
        }
      } catch {
        setPrereqs(null);
      }
    }
    setStage("guidance");
  }, [platform, onDetected]);

  useEffect(() => {
    void check();
  }, [check]);

  // Auto-advance when the engine is ready.
  useEffect(() => {
    if (stage === "ready" && !autoAdvanced) {
      setAutoAdvanced(true);
      const timer = setTimeout(onNext, 2000);
      return () => clearTimeout(timer);
    }
  }, [stage, onNext, autoAdvanced]);

  // Windows only: install the bundled Podman if needed, then create and start
  // its machine. `install_podman` refuses to run anywhere else.
  const handleWindowsInstall = async (alreadyInstalled: boolean) => {
    setInstallError(null);
    setStage(alreadyInstalled ? "initializing" : "installing");
    try {
      await installPodman(state.cpuCores, state.memoryMb, state.diskGb);
      let found: ContainerRuntimeDetection | null = null;
      try {
        found = await detectContainerRuntime();
      } catch {
        // Fall through: the installer reported success, which is what matters.
      }
      if (found && !found.responding) {
        // Installed and started, but the probe disagrees — say so rather than
        // claiming readiness.
        setDetection(found);
        onDetected(false);
        setInstallError(found.detail || "Podman was set up but is not answering yet.");
        setStage("error");
        return;
      }
      setDetection(found ?? { backend: "podman", version: "", binary_path: "", responding: true, detail: "" });
      onDetected(true);
      setStage("ready");
    } catch (err) {
      setStage("error");
      setInstallError(String(err));
    }
  };

  const header = (subtitle: string) => (
    <div className="text-center space-y-2">
      <h2 className="text-2xl font-bold">Container Runtime</h2>
      <p className="text-muted-foreground">{subtitle}</p>
    </div>
  );

  const skipButton = (
    <Button variant="outline" onClick={onNext}>
      {SKIP_CONTAINER_LABEL}
    </Button>
  );

  if (stage === "ready") {
    const label = detection ? backendLabel(detection.backend) : "Podman";
    const version = detection?.version ? ` ${detection.version}` : "";
    return (
      <div className="space-y-6 max-w-md mx-auto">
        {header("Container tasks can run on this machine.")}
        <Card>
          <CardContent className="flex items-center gap-3 pt-6">
            <div className="h-8 w-8 rounded-full bg-green-100 flex items-center justify-center text-green-600">
              ✓
            </div>
            <div>
              <div className="font-medium">
                Ready ({label}
                {version})
              </div>
              <div className="text-sm text-muted-foreground">
                Detected and answering. You can allow container tasks per head in the next step.
              </div>
            </div>
          </CardContent>
        </Card>
        <div className="flex justify-between">
          <Button variant="ghost" onClick={onBack}>Back</Button>
          <Button onClick={onNext}>Next</Button>
        </div>
      </div>
    );
  }

  if (stage === "wsl_required") {
    return (
      <div className="space-y-6 max-w-md mx-auto">
        {header("One more thing before container tasks can run here.")}
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Enable WSL2</CardTitle>
            <CardDescription>
              Windows Subsystem for Linux (WSL2) is required to run containers.
              This is a one-time setup.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="rounded-md bg-muted p-3 text-xs font-mono whitespace-pre-line">
              {"1. Open PowerShell as Administrator\n2. Run: wsl --install\n3. Restart your computer\n4. Re-open Lettuce Compute"}
            </div>
            <p className="text-xs text-muted-foreground">
              After WSL2 is enabled, run setup again and Lettuce will install and configure the
              container runtime for you.
            </p>
          </CardContent>
        </Card>
        <div className="flex justify-between">
          <Button variant="ghost" onClick={onBack}>Back</Button>
          {skipButton}
        </div>
      </div>
    );
  }

  if (stage === "installing" || stage === "initializing") {
    const stages = [
      { key: "installing", label: "Installing Podman" },
      { key: "initializing", label: "Setting up the container VM" },
    ];
    const currentIdx = stages.findIndex((s) => s.key === stage);
    return (
      <div className="space-y-6 max-w-md mx-auto">
        <div className="text-center space-y-2">
          <h2 className="text-2xl font-bold">Setting Up Containers</h2>
          <p className="text-muted-foreground">
            {stage === "installing"
              ? "Installing Podman... This may take a minute."
              : "Creating and starting the Podman machine..."}
          </p>
        </div>
        <Card>
          <CardContent className="pt-6 space-y-4">
            {stages.map((s, i) => (
              <div key={s.key} className="flex items-center gap-3">
                {i < currentIdx ? (
                  <div className="h-6 w-6 rounded-full bg-green-100 flex items-center justify-center text-green-600 text-xs">
                    ✓
                  </div>
                ) : i === currentIdx ? (
                  <div className="animate-spin h-6 w-6 border-2 border-primary border-t-transparent rounded-full" />
                ) : (
                  <div className="h-6 w-6 rounded-full bg-muted" />
                )}
                <span className={cn("text-sm", i <= currentIdx ? "font-medium" : "text-muted-foreground")}>
                  {s.label}
                </span>
              </div>
            ))}
          </CardContent>
        </Card>
        <p className="text-xs text-muted-foreground text-center">
          This is a one-time setup and may take a few minutes.
        </p>
      </div>
    );
  }

  if (stage === "error") {
    return (
      <div className="space-y-6 max-w-md mx-auto">
        {header("Something went wrong while checking or setting up the container runtime.")}
        <div className="rounded-md bg-destructive/10 p-3 text-sm text-destructive">
          {installError}
        </div>
        <div className="flex justify-between">
          <Button variant="ghost" onClick={onBack}>Back</Button>
          <div className="flex gap-2">
            {skipButton}
            <Button onClick={check}>Retry</Button>
          </div>
        </div>
      </div>
    );
  }

  if (stage === "guidance") {
    const installed = detection !== null && detection.backend !== "none";
    const label = detection ? backendLabel(detection.backend) : "";
    const podmanInstalled = detection?.backend === "podman";

    let body: ReactNode;
    let action: ReactNode = null;

    if (platform === "windows") {
      if (podmanInstalled) {
        body = (
          <>
            <p>{detection?.detail || "Podman is installed but its machine is not running."}</p>
            <p>
              Lettuce can create and start the Podman machine (a small Linux VM) for you.
              No administrator rights are needed.
            </p>
          </>
        );
        action = <Button onClick={() => handleWindowsInstall(true)}>Set Up</Button>;
      } else if (installed) {
        body = (
          <>
            <p>{detection?.detail || `${label} is installed but not running.`}</p>
            <p>Start Docker Desktop and wait until it reports that the engine is running, then check again.</p>
          </>
        );
      } else {
        body = (
          <>
            <p>
              No Docker or Podman was found. Lettuce can install Podman, an open-source container
              engine, from the installer bundled with this app (about 26 MB, no download) and create
              a small Linux VM for it. No administrator rights are needed.
            </p>
            {prereqs && prereqs.podman_installed && (
              <p className="text-muted-foreground">
                A Podman installation was found at {prereqs.podman_path}; only the machine setup will run.
              </p>
            )}
          </>
        );
        action = (
          <Button onClick={() => handleWindowsInstall(Boolean(prereqs?.podman_installed))}>
            Install & Set Up
          </Button>
        );
      }
    } else if (platform === "macos") {
      if (installed) {
        body = (
          <>
            <p>{detection?.detail || `${label} is installed but not running.`}</p>
            {podmanInstalled ? (
              <p>
                Start it from Podman Desktop, or in a terminal run{" "}
                <code className="rounded bg-muted px-1.5 py-0.5 text-xs">podman machine init</code>{" "}
                (first time only) and then{" "}
                <code className="rounded bg-muted px-1.5 py-0.5 text-xs">podman machine start</code>.
                Then check again.
              </p>
            ) : (
              <p>Start Docker Desktop and wait until it reports that the engine is running, then check again.</p>
            )}
          </>
        );
      } else {
        body = (
          <>
            <p>No Docker or Podman was found on this Mac.</p>
            <p>
              Install <span className="font-medium">Podman Desktop</span> or{" "}
              <span className="font-medium">Docker Desktop</span>, open it, and make sure its
              machine (a small Linux VM) is started. Then check again.
            </p>
          </>
        );
      }
    } else {
      // Linux
      if (installed) {
        body = (
          <>
            <p>{detection?.detail || `${label} is installed but not running.`}</p>
            {podmanInstalled ? (
              <p>
                Start the Podman API socket as your normal user (not with sudo):{" "}
                <code className="rounded bg-muted px-1.5 py-0.5 text-xs">
                  systemctl --user enable --now podman.socket
                </code>
                . Then check again.
              </p>
            ) : (
              <p>
                Start the Docker service (for example{" "}
                <code className="rounded bg-muted px-1.5 py-0.5 text-xs">sudo systemctl start docker</code>)
                and make sure your user is allowed to use it. Then check again.
              </p>
            )}
          </>
        );
      } else {
        body = (
          <>
            <p>No Docker or Podman was found.</p>
            <p>
              Install Podman from your distribution's packages (for example{" "}
              <code className="rounded bg-muted px-1.5 py-0.5 text-xs">sudo apt install podman</code>{" "}
              or <code className="rounded bg-muted px-1.5 py-0.5 text-xs">sudo dnf install podman</code>),
              then start its API socket as your normal user:
            </p>
            <div className="rounded-md bg-muted p-3 text-xs font-mono whitespace-pre-line">
              {"systemctl --user enable --now podman.socket\nloginctl enable-linger \"$USER\""}
            </div>
            <p>Docker also works when your user can reach its socket. Then check again.</p>
          </>
        );
      }
    }

    return (
      <div className="space-y-6 max-w-md mx-auto">
        {header(
          installed
            ? `${label} is installed but not answering.`
            : "Container tasks need Docker or Podman. WASM and native tasks run without it."
        )}
        <Card>
          <CardHeader>
            <CardTitle className="text-base">
              {installed ? `${label} found${detection?.version ? ` (${detection.version})` : ""}` : "No container runtime detected"}
            </CardTitle>
          </CardHeader>
          <CardContent className="space-y-3 text-sm">{body}</CardContent>
        </Card>
        <div className="flex justify-between">
          <Button variant="ghost" onClick={onBack}>Back</Button>
          <div className="flex gap-2">
            {skipButton}
            {action ?? (
              <Button onClick={check}>Check again</Button>
            )}
          </div>
        </div>
      </div>
    );
  }

  // checking
  return (
    <div className="space-y-6 max-w-md mx-auto">
      {header("Checking your system...")}
      <div className="flex justify-center">
        <div className="animate-spin h-8 w-8 border-2 border-primary border-t-transparent rounded-full" />
      </div>
      <div className="flex justify-between">
        <Button variant="ghost" onClick={onBack}>Back</Button>
        {skipButton}
      </div>
    </div>
  );
}

function ConnectStep({
  state,
  onChange,
  onComplete,
  onSkip,
  onBack,
  isSubmitting,
}: {
  state: WizardState;
  onChange: (s: Partial<WizardState>) => void;
  onComplete: () => void;
  onSkip: () => void;
  onBack: () => void;
  isSubmitting: boolean;
}) {
  const [testResult, setTestResult] = useState<"success" | "error" | null>(null);
  const [isTesting, setIsTesting] = useState(false);

  const hasUrl = state.serverUrl.trim() !== "";
  const leafsOffered = state.headPreview !== null && state.headPreview.leafs.length > 0;
  const noLeafSelected = leafsOffered && state.enabledLeafSlugs.length === 0;
  const canStart = !hasUrl || (state.connectionOk && !noLeafSelected);

  const testConnection = async () => {
    if (!hasUrl) return;
    setIsTesting(true);
    setTestResult(null);
    onChange({ headPreview: null, enabledLeafSlugs: [], connectionOk: false });
    try {
      const health = await testServerConnection(state.serverUrl.trim());
      if (health.status !== "healthy") {
        setTestResult("error");
        return;
      }
      setTestResult("success");
      let preview: HeadPreview | null = null;
      let slugs: string[] = [];
      try {
        const head = await fetchHeadInfo(state.serverUrl.trim());
        const activeLeafs = head.leafs.filter((l) => l.state === "ACTIVE");
        preview = {
          name: head.name,
          description: head.description,
          leafs: activeLeafs.map((l) => ({
            slug: l.slug,
            name: l.name,
            research_area: l.research_area,
          })),
        };
        slugs = activeLeafs.map((l) => l.slug);
      } catch {
        // The head answered its health check but not its info endpoint;
        // attaching still works, only the preview is missing.
      }
      // A fresh head means a fresh consent: container defaults to allowed
      // only when an engine actually answered, native is always off.
      onChange({
        connectionOk: true,
        headPreview: preview,
        enabledLeafSlugs: slugs,
        trustContainer: state.containerRuntimeDetected,
        trustNative: false,
      });
    } catch {
      setTestResult("error");
    } finally {
      setIsTesting(false);
    }
  };

  const toggleLeaf = (slug: string) => {
    const current = state.enabledLeafSlugs;
    if (current.includes(slug)) {
      onChange({ enabledLeafSlugs: current.filter((s) => s !== slug) });
    } else {
      onChange({ enabledLeafSlugs: [...current, slug] });
    }
  };

  return (
    <div className="space-y-6 max-w-md mx-auto">
      <div className="text-center space-y-2">
        <h2 className="text-2xl font-bold">Connect</h2>
        <p className="text-muted-foreground">
          Add a server to start contributing compute.
        </p>
      </div>

      <div className="space-y-3">
        <Input
          placeholder="https://compute.example.org"
          value={state.serverUrl}
          onChange={(e) => {
            onChange({
              serverUrl: e.target.value,
              connectionOk: false,
              headPreview: null,
              enabledLeafSlugs: [],
            });
            setTestResult(null);
          }}
        />
        <div className="flex items-center gap-3">
          <Button
            variant="outline"
            onClick={testConnection}
            disabled={!hasUrl || isTesting}
          >
            {isTesting ? "Testing..." : "Test Connection"}
          </Button>
          {testResult === "success" && (
            <span className="text-sm text-green-600">Connected</span>
          )}
          {testResult === "error" && (
            <span className="text-sm text-destructive">Connection failed</span>
          )}
        </div>
        {hasUrl && !state.connectionOk && testResult !== "error" && (
          <p className="text-xs text-muted-foreground">
            Test the connection to see what this head offers and choose what it may run.
          </p>
        )}
      </div>

      {/* Head info preview */}
      {state.headPreview && (
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-base">{state.headPreview.name}</CardTitle>
            {state.headPreview.description && (
              <CardDescription className="line-clamp-2">
                {state.headPreview.description}
              </CardDescription>
            )}
          </CardHeader>
          {state.headPreview.leafs.length > 0 && (
            <CardContent className="space-y-2">
              <p className="text-xs font-medium text-muted-foreground">
                Select leafs to compute:
              </p>
              {state.headPreview.leafs.map((leaf) => (
                <label
                  key={leaf.slug}
                  className="flex items-center gap-2 text-sm cursor-pointer"
                >
                  <input
                    type="checkbox"
                    aria-label={leaf.name}
                    checked={state.enabledLeafSlugs.includes(leaf.slug)}
                    onChange={() => toggleLeaf(leaf.slug)}
                    className="h-4 w-4 rounded border-input accent-primary"
                  />
                  <span>{leaf.name}</span>
                  {leaf.research_area && (
                    <span className="inline-flex items-center rounded-full bg-secondary px-1.5 py-0.5 text-[10px]">
                      {leaf.research_area}
                    </span>
                  )}
                </label>
              ))}
              {noLeafSelected && (
                <p className="text-xs text-destructive">Select at least one leaf, or skip adding a server.</p>
              )}
            </CardContent>
          )}
        </Card>
      )}

      {/* Runtime-trust consent: what this head may run here beyond WASM. The
          same block the Projects page uses; the wizard only adds a heading
          and explains why container tasks are missing when its own probe in
          the previous step found no engine. */}
      {state.connectionOk && (
        <Card>
          <CardHeader className="pb-2">
            <CardTitle className="text-base">What may this head run on your machine?</CardTitle>
          </CardHeader>
          <CardContent>
            <RuntimeTrustFields
              headName={state.headPreview?.name || state.serverUrl.trim()}
              value={{ container: state.trustContainer, native: state.trustNative }}
              onChange={(next) => onChange({ trustContainer: next.container, trustNative: next.native })}
              containerAvailable={state.containerRuntimeDetected}
              containerUnavailableNote={WIZARD_CONTAINER_UNAVAILABLE_NOTE}
              disabled={isSubmitting}
            />
          </CardContent>
        </Card>
      )}

      <div className="flex items-center justify-between">
        <Button variant="ghost" onClick={onBack}>
          Back
        </Button>
        <div className="flex items-center gap-3">
          <button
            type="button"
            onClick={onSkip}
            className="text-sm text-muted-foreground hover:underline"
            disabled={isSubmitting}
          >
            Skip — I'll add one later
          </button>
          <Button onClick={onComplete} disabled={isSubmitting || !canStart}>
            {isSubmitting ? "Setting up..." : "Start Contributing"}
          </Button>
        </div>
      </div>
    </div>
  );
}

export function SetupWizard({ onComplete }: WizardProps) {
  const [step, setStep] = useState(0);
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [state, setState] = useState<WizardState>({
    cpuCores: Math.max(1, Math.floor(maxCpuCores / 2)),
    memoryMb: Math.round(FALLBACK_TOTAL_MEMORY_MB * 0.5),
    gpuVramPct: 50,
    diskGb: 10,
    totalMemoryMb: FALLBACK_TOTAL_MEMORY_MB,
    scheduleMode: "always",
    idleThresholdMins: 5,
    scheduleFromHour: 20,
    scheduleToHour: 6,
    scheduleDays: [...WEEKDAYS],
    containerRuntimeDetected: false,
    serverUrl: "",
    connectionOk: false,
    headPreview: null,
    enabledLeafSlugs: [],
    trustContainer: false,
    trustNative: false,
  });

  const update = useCallback((partial: Partial<WizardState>) => {
    setState((prev) => ({ ...prev, ...partial }));
  }, []);

  const setContainerDetected = useCallback(
    (detected: boolean) => update({ containerRuntimeDetected: detected }),
    [update]
  );

  // Where setup will put the keys: the app's data directory (`~/.lettuce`, or
  // the `LETTUCE_DATA_DIR` override). Shown on the Identity step so the folder
  // named there is the real one.
  const [dataDir, setDataDir] = useState("~/.lettuce");
  useEffect(() => {
    let cancelled = false;
    getDataDir()
      .then((dir) => {
        if (!cancelled && typeof dir === "string" && dir) setDataDir(dir);
      })
      .catch(() => {
        // Keep the conventional default.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Detect real total RAM once on mount and size the memory slider / default to
  // it. Without this the slider was capped at ~7.4 GB (hardcoded 8 GB), which is
  // below the memory floor of large leaves (e.g. extract2 ≥28 GB), so the
  // volunteer was never offered that work. Falls back to the initial value.
  useEffect(() => {
    let cancelled = false;
    getSystemMemoryMb()
      .then((mb) => {
        if (cancelled || !mb || mb <= 0) return;
        setState((prev) => ({
          ...prev,
          totalMemoryMb: mb,
          memoryMb: Math.round(mb * 0.5),
        }));
      })
      .catch(() => {
        // Detection failed — keep the fallback already in state.
      });
    return () => {
      cancelled = true;
    };
  }, []);

  /**
   * Run `init` with everything chosen so far. `withServer` false is the
   * "Skip — I'll add one later" path: no head is attached and no trust is
   * recorded, whatever the URL field holds.
   */
  const handleComplete = async (withServer: boolean) => {
    setIsSubmitting(true);
    setError(null);
    const serverUrl = state.serverUrl.trim();
    const hasServer = withServer && serverUrl !== "";
    const schedule = buildInitSchedule({
      mode: state.scheduleMode,
      idleThresholdMins: state.idleThresholdMins,
      window: {
        from_hour: state.scheduleFromHour,
        to_hour: state.scheduleToHour,
        days: state.scheduleDays,
      },
    });
    const trust: Array<"container" | "native"> = [];
    if (hasServer && state.trustContainer && state.containerRuntimeDetected) trust.push("container");
    if (hasServer && state.trustNative) trust.push("native");
    const partialSelection =
      hasServer &&
      state.headPreview !== null &&
      state.enabledLeafSlugs.length > 0 &&
      state.enabledLeafSlugs.length < state.headPreview.leafs.length;
    try {
      await runInit({
        cpu_cores: state.cpuCores,
        memory_mb: state.memoryMb,
        gpu_vram_pct: state.gpuVramPct,
        disk_gb: state.diskGb,
        schedule_mode: schedule.schedule_mode,
        idle_threshold_mins: schedule.idle_threshold_mins,
        schedule_window: schedule.schedule_window,
        server_url: hasServer ? serverUrl : null,
        trust,
        enabled_leafs: partialSelection ? state.enabledLeafSlugs : null,
      });
      onComplete();
    } catch (err) {
      setError(String(err));
      setIsSubmitting(false);
    }
  };

  return (
    <div className="flex min-h-screen items-center justify-center p-8">
      <div className="w-full max-w-lg">
        <StepIndicator current={step} total={6} />

        {error && (
          <div className="mb-4 rounded-md bg-destructive/10 p-3 text-sm text-destructive">
            {error}
          </div>
        )}

        {step === 0 && <WelcomeStep onNext={() => setStep(1)} />}
        {step === 1 && (
          <IdentityStep
            dataDir={dataDir}
            onNext={() => setStep(2)}
            onBack={() => setStep(0)}
          />
        )}
        {step === 2 && (
          <ResourcesStep
            state={state}
            onChange={update}
            onNext={() => setStep(3)}
            onBack={() => setStep(1)}
          />
        )}
        {step === 3 && (
          <ScheduleStep
            state={state}
            onChange={update}
            onNext={() => setStep(4)}
            onBack={() => setStep(2)}
          />
        )}
        {step === 4 && (
          <ContainerRuntimeStep
            state={state}
            onDetected={setContainerDetected}
            onNext={() => setStep(5)}
            onBack={() => setStep(3)}
          />
        )}
        {step === 5 && (
          <ConnectStep
            state={state}
            onChange={update}
            onComplete={() => handleComplete(true)}
            onSkip={() => handleComplete(false)}
            onBack={() => setStep(4)}
            isSubmitting={isSubmitting}
          />
        )}
      </div>
    </div>
  );
}
