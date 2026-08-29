import { useState, useEffect } from "react";
import { invoke } from "@tauri-apps/api/core";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Slider } from "@/components/ui/slider";
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from "@/components/ui/card";
import { cn, detectPlatform } from "@/lib/utils";
import lettuceLeaf from "@/assets/lettuce-leaf.png";
import type { HeadPreview, ContainerRuntimeStatus, PodmanPrerequisites } from "@/api/client";
import { getContainerRuntimeStatus, checkPodmanPrerequisites, installPodman, getSystemMemoryMb } from "@/api/client";

interface WizardProps {
  onComplete: () => void;
}

type ScheduleMode = "ALWAYS" | "IDLE" | "CRON";

interface WizardState {
  cpuCores: number;
  memoryMb: number;
  gpuVramPct: number;
  diskGb: number;
  totalMemoryMb: number;
  scheduleMode: ScheduleMode;
  idleThresholdMins: number;
  scheduleStartHour: number;
  scheduleEndHour: number;
  serverUrl: string;
  headPreview: HeadPreview | null;
  enabledLeafSlugs: string[];
}

const maxCpuCores = navigator.hardwareConcurrency || 4;
// Fallback when OS memory detection fails. Real total RAM is detected on mount
// (get_system_memory_mb) and stored in wizard state — the old hardcoded 8 GB
// capped the memory slider at ~7.4 GB, below the floor of large-memory leaves
// (e.g. extract2 needs ≥28 GB), so those volunteers were never matched to work.
const FALLBACK_TOTAL_MEMORY_MB = 8192;

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
  onNext,
  onBack,
}: {
  onNext: () => void;
  onBack: () => void;
}) {
  return (
    <div className="space-y-6 max-w-md mx-auto">
      <div className="text-center space-y-2">
        <h2 className="text-2xl font-bold">Identity</h2>
        <p className="text-muted-foreground">
          We'll generate a cryptographic identity for you when setup completes.
        </p>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">Cryptographic Identity</CardTitle>
          <CardDescription>
            Your Ed25519 keypair will be generated automatically.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <div className="rounded-md bg-muted p-3 text-xs font-mono text-muted-foreground">
            Key will be generated at ~/.lettuce/identity.key
          </div>
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
  const modes: { value: ScheduleMode; label: string; desc: string }[] = [
    { value: "ALWAYS", label: "Always On", desc: "Compute whenever possible" },
    { value: "IDLE", label: "When Idle", desc: "Only when you're not using your computer" },
    { value: "CRON", label: "Scheduled", desc: "During specific hours" },
  ];

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

      {state.scheduleMode === "IDLE" && (
        <div className="space-y-2">
          <div className="flex justify-between text-sm">
            <span>Idle threshold</span>
            <span className="font-medium">{state.idleThresholdMins} min</span>
          </div>
          <Slider
            min={1}
            max={30}
            value={state.idleThresholdMins}
            onChange={(v) => onChange({ idleThresholdMins: v })}
          />
        </div>
      )}

      {state.scheduleMode === "CRON" && (
        <div className="space-y-4">
          <div className="space-y-2">
            <div className="flex justify-between text-sm">
              <span>Start hour</span>
              <span className="font-medium">{state.scheduleStartHour}:00</span>
            </div>
            <Slider
              min={0}
              max={23}
              value={state.scheduleStartHour}
              onChange={(v) => onChange({ scheduleStartHour: v })}
            />
          </div>
          <div className="space-y-2">
            <div className="flex justify-between text-sm">
              <span>End hour</span>
              <span className="font-medium">{state.scheduleEndHour}:00</span>
            </div>
            <Slider
              min={0}
              max={23}
              value={state.scheduleEndHour}
              onChange={(v) => onChange({ scheduleEndHour: v })}
            />
          </div>
        </div>
      )}

      <div className="flex justify-between">
        <Button variant="ghost" onClick={onBack}>
          Back
        </Button>
        <Button onClick={onNext}>Next</Button>
      </div>
    </div>
  );
}

type InstallStage = "checking" | "prerequisites" | "installing" | "initializing" | "starting" | "done" | "error" | "wsl_required";

function ContainerRuntimeStep({
  state,
  onNext,
  onBack,
}: {
  state: WizardState;
  onNext: () => void;
  onBack: () => void;
}) {
  const [runtimeStatus, setRuntimeStatus] =
    useState<ContainerRuntimeStatus | null>(null);
  const [prereqs, setPrereqs] = useState<PodmanPrerequisites | null>(null);
  const [installStage, setInstallStage] = useState<InstallStage>("checking");
  const [stageMessage, setStageMessage] = useState("Checking system...");
  const [installError, setInstallError] = useState<string | null>(null);
  const [autoAdvanced, setAutoAdvanced] = useState(false);

  const platform = detectPlatform();

  // On mount: check runtime status, then check prerequisites
  useEffect(() => {
    let cancelled = false;
    const check = async () => {
      // First check if runtime is already available (Docker running, or Podman already set up)
      try {
        const status = await getContainerRuntimeStatus();
        if (!cancelled) {
          setRuntimeStatus(status);
          if (status.status === "running") {
            setInstallStage("done");
            return;
          }
          if (status.status === "not_initialized") {
            // Podman exists but needs machine init — go straight to setup
            setInstallStage("prerequisites");
            setPrereqs({
              wsl_available: true,
              podman_installed: true,
              podman_path: null,
              needs_install: false,
            });
            return;
          }
        }
      } catch {
        // Daemon not ready yet — expected during wizard before init
      }

      // Check Podman prerequisites on Windows
      if (platform === "windows") {
        try {
          const p = await checkPodmanPrerequisites();
          if (!cancelled) {
            setPrereqs(p);
            if (!p.wsl_available) {
              setInstallStage("wsl_required");
            } else if (p.podman_installed) {
              setInstallStage("prerequisites");
            } else {
              setInstallStage("prerequisites");
            }
          }
        } catch {
          if (!cancelled) setInstallStage("prerequisites");
        }
      } else if (platform === "linux") {
        // Linux should have bundled Podman
        setInstallStage("done");
      } else {
        // macOS — fall back to guidance
        setInstallStage("prerequisites");
      }
    };
    check();
    return () => { cancelled = true; };
  }, [platform]);

  // Auto-advance when done
  useEffect(() => {
    if (installStage === "done" && !autoAdvanced) {
      setAutoAdvanced(true);
      const timer = setTimeout(onNext, 2000);
      return () => clearTimeout(timer);
    }
  }, [installStage, onNext, autoAdvanced]);

  // Automated install flow for Windows
  const handleAutoInstall = async () => {
    setInstallError(null);
    try {
      setInstallStage("installing");
      setStageMessage("Installing Podman... This may take a minute.");

      await installPodman(state.cpuCores, state.memoryMb, state.diskGb);

      setInstallStage("done");
      setStageMessage("Container runtime is ready!");

      // Refresh status from daemon
      try {
        const status = await getContainerRuntimeStatus();
        setRuntimeStatus(status);
      } catch {
        // Daemon may need restart to detect new Podman — that's OK
      }
    } catch (err) {
      setInstallStage("error");
      setInstallError(String(err));
    }
  };

  // Manual setup for when Podman exists but machine needs init
  // Uses installPodman (which calls podman directly) instead of setupContainerRuntime
  // (which requires the daemon to be running — but the daemon isn't started until
  // the final wizard step calls run_init).
  const handleSetup = async () => {
    setInstallError(null);
    setInstallStage("initializing");
    setStageMessage("Initializing container runtime...");
    try {
      await installPodman(state.cpuCores, state.memoryMb, state.diskGb);
      setInstallStage("done");
      setStageMessage("Container runtime is ready!");

      // Refresh status from daemon if available
      try {
        const status = await getContainerRuntimeStatus();
        setRuntimeStatus(status);
      } catch {
        // Daemon not running yet — expected during wizard
      }
    } catch (err) {
      setInstallStage("error");
      setInstallError(String(err));
    }
  };

  // --- Render states ---

  // Already running
  if (installStage === "done" || runtimeStatus?.status === "running") {
    return (
      <div className="space-y-6 max-w-md mx-auto">
        <div className="text-center space-y-2">
          <h2 className="text-2xl font-bold">Container Runtime</h2>
          <p className="text-muted-foreground">
            Container runtime is ready for running projects.
          </p>
        </div>
        <Card>
          <CardContent className="flex items-center gap-3 pt-6">
            <div className="h-8 w-8 rounded-full bg-green-100 flex items-center justify-center text-green-600">
              ✓
            </div>
            <div>
              <div className="font-medium">
                {runtimeStatus?.backend === "docker" ? "Docker" : "Podman"}{" "}
                {runtimeStatus?.version && `v${runtimeStatus.version}`}
              </div>
              <div className="text-sm text-muted-foreground">
                Container runtime ready
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

  // WSL2 not available — user needs to enable it (requires admin + reboot)
  if (installStage === "wsl_required") {
    return (
      <div className="space-y-6 max-w-md mx-auto">
        <div className="text-center space-y-2">
          <h2 className="text-2xl font-bold">Container Runtime</h2>
          <p className="text-muted-foreground">
            One more thing before we can run container projects.
          </p>
        </div>
        <Card>
          <CardHeader>
            <CardTitle className="text-base">Enable WSL2</CardTitle>
            <CardDescription>
              Windows Subsystem for Linux (WSL2) is required to run containers.
              This is a one-time setup.
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="rounded-md bg-muted p-3 text-xs font-mono">
              1. Open PowerShell as Administrator{"\n"}
              2. Run: <span className="text-primary">wsl --install</span>{"\n"}
              3. Restart your computer{"\n"}
              4. Re-open Lettuce Compute
            </div>
            <p className="text-xs text-muted-foreground">
              After enabling WSL2, Lettuce will automatically install and
              configure the container runtime on next launch.
            </p>
          </CardContent>
        </Card>
        <div className="flex justify-between">
          <Button variant="ghost" onClick={onBack}>Back</Button>
          <Button variant="outline" onClick={onNext}>
            Skip — native projects only
          </Button>
        </div>
      </div>
    );
  }

  // Installing / initializing / starting — progress view
  if (installStage === "installing" || installStage === "initializing" || installStage === "starting") {
    const stages = [
      { key: "installing", label: "Installing Podman" },
      { key: "initializing", label: "Setting up container VM" },
      { key: "starting", label: "Starting runtime" },
    ];
    const currentIdx = stages.findIndex((s) => s.key === installStage);

    return (
      <div className="space-y-6 max-w-md mx-auto">
        <div className="text-center space-y-2">
          <h2 className="text-2xl font-bold">Setting Up Containers</h2>
          <p className="text-muted-foreground">{stageMessage}</p>
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
                <span className={cn(
                  "text-sm",
                  i <= currentIdx ? "font-medium" : "text-muted-foreground"
                )}>
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

  // Error state
  if (installStage === "error") {
    return (
      <div className="space-y-6 max-w-md mx-auto">
        <div className="text-center space-y-2">
          <h2 className="text-2xl font-bold">Container Runtime</h2>
          <p className="text-muted-foreground">
            Something went wrong during setup.
          </p>
        </div>
        <div className="rounded-md bg-destructive/10 p-3 text-sm text-destructive">
          {installError}
        </div>
        <div className="flex justify-between">
          <Button variant="ghost" onClick={onBack}>Back</Button>
          <div className="flex gap-2">
            <Button variant="outline" onClick={onNext}>
              Skip
            </Button>
            <Button onClick={() => {
              setInstallStage("prerequisites");
              setInstallError(null);
            }}>
              Retry
            </Button>
          </div>
        </div>
      </div>
    );
  }

  // Prerequisites / ready to install — main action screen
  if (installStage === "prerequisites") {
    // macOS: fall back to manual guidance (no bundled installer yet)
    if (platform === "macos") {
      return (
        <div className="space-y-6 max-w-md mx-auto">
          <div className="text-center space-y-2">
            <h2 className="text-2xl font-bold">Container Runtime</h2>
            <p className="text-muted-foreground">
              A container runtime is needed to run containerized projects.
            </p>
          </div>
          <Card>
            <CardHeader>
              <CardTitle className="text-base">Install Podman</CardTitle>
              <CardDescription>
                Install Podman via Homebrew:{" "}
                <code className="rounded bg-muted px-1.5 py-0.5 text-xs">
                  brew install podman
                </code>
                . After installation, restart Lettuce Compute.
              </CardDescription>
            </CardHeader>
          </Card>
          <div className="flex justify-between">
            <Button variant="ghost" onClick={onBack}>Back</Button>
            <Button variant="outline" onClick={onNext}>Skip</Button>
          </div>
        </div>
      );
    }

    // Linux: bundled, shouldn't need install
    if (platform === "linux") {
      return (
        <div className="space-y-6 max-w-md mx-auto">
          <div className="text-center space-y-2">
            <h2 className="text-2xl font-bold">Container Runtime</h2>
            <p className="text-muted-foreground">
              Bundled Podman is available. Setting up...
            </p>
          </div>
          <div className="flex justify-between">
            <Button variant="ghost" onClick={onBack}>Back</Button>
            <Button onClick={handleSetup}>Set Up</Button>
          </div>
        </div>
      );
    }

    // Windows: automated install
    const alreadyInstalled = prereqs?.podman_installed;

    return (
      <div className="space-y-6 max-w-md mx-auto">
        <div className="text-center space-y-2">
          <h2 className="text-2xl font-bold">Container Runtime</h2>
          <p className="text-muted-foreground">
            {alreadyInstalled
              ? "Podman is installed. We'll set up the container environment."
              : "We'll install a lightweight container runtime so you can run containerized science projects."}
          </p>
        </div>
        <Card>
          <CardContent className="pt-6 space-y-3">
            <div className="flex items-start gap-3">
              <div className="h-8 w-8 rounded-full bg-primary/10 flex items-center justify-center text-primary shrink-0 mt-0.5">
                📦
              </div>
              <div>
                <div className="font-medium text-sm">
                  {alreadyInstalled ? "Initialize Container VM" : "Podman Container Runtime"}
                </div>
                <div className="text-xs text-muted-foreground">
                  {alreadyInstalled
                    ? "Creates a lightweight Linux VM for running containers."
                    : "Installs Podman and creates a lightweight Linux VM for running containers. No admin privileges required."}
                </div>
              </div>
            </div>
            {!alreadyInstalled && (
              <div className="text-xs text-muted-foreground border-t pt-2">
                Podman is an open-source container engine by Red Hat. ~26 MB download, already bundled with this app.
              </div>
            )}
          </CardContent>
        </Card>
        <div className="flex justify-between">
          <Button variant="ghost" onClick={onBack}>Back</Button>
          <div className="flex gap-2">
            <Button variant="outline" onClick={onNext}>
              Skip
            </Button>
            <Button onClick={alreadyInstalled ? handleSetup : handleAutoInstall}>
              {alreadyInstalled ? "Set Up" : "Install & Set Up"}
            </Button>
          </div>
        </div>
      </div>
    );
  }

  // Checking / loading state
  return (
    <div className="space-y-6 max-w-md mx-auto">
      <div className="text-center space-y-2">
        <h2 className="text-2xl font-bold">Container Runtime</h2>
        <p className="text-muted-foreground">
          Checking your system...
        </p>
      </div>
      <div className="flex justify-center">
        <div className="animate-spin h-8 w-8 border-2 border-primary border-t-transparent rounded-full" />
      </div>
      <div className="flex justify-between">
        <Button variant="ghost" onClick={onBack}>Back</Button>
        <Button variant="outline" onClick={onNext}>Skip</Button>
      </div>
    </div>
  );
}

function ConnectStep({
  state,
  onChange,
  onComplete,
  onBack,
  isSubmitting,
}: {
  state: WizardState;
  onChange: (s: Partial<WizardState>) => void;
  onComplete: () => void;
  onBack: () => void;
  isSubmitting: boolean;
}) {
  const [testResult, setTestResult] = useState<string | null>(null);
  const [isTesting, setIsTesting] = useState(false);

  const testConnection = async () => {
    if (!state.serverUrl) return;
    setIsTesting(true);
    setTestResult(null);
    onChange({ headPreview: null, enabledLeafSlugs: [] });
    try {
      const health = await invoke<{ status: string }>("test_server_connection", { url: state.serverUrl });
      if (health.status === "healthy") {
        setTestResult("success");

        // Fetch head info for preview
        try {
          const headData = await invoke<{ name: string; description: string; leafs: Array<{ slug: string; name: string; research_area: string; state: string }> }>("fetch_head_info", { url: state.serverUrl });
          const activeLeafs = (headData.leafs ?? []).filter(
            (l) => l.state === "ACTIVE"
          );
          onChange({
            headPreview: {
              name: headData.name ?? "",
              description: headData.description ?? "",
              leafs: activeLeafs,
            },
            enabledLeafSlugs: activeLeafs.map((l) => l.slug),
          });
        } catch {
          // Head info not available, proceed without preview
        }
      } else {
        setTestResult("error");
      }
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
            onChange({ serverUrl: e.target.value, headPreview: null, enabledLeafSlugs: [] });
            setTestResult(null);
          }}
        />
        <div className="flex items-center gap-3">
          <Button
            variant="outline"
            onClick={testConnection}
            disabled={!state.serverUrl || isTesting}
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
                    checked={state.enabledLeafSlugs.includes(leaf.slug)}
                    onChange={() => toggleLeaf(leaf.slug)}
                    className="h-4 w-4 rounded border-input accent-primary"
                  />
                  <span>{leaf.name}</span>
                  <span className="inline-flex items-center rounded-full bg-secondary px-1.5 py-0.5 text-[10px]">
                    {leaf.research_area}
                  </span>
                </label>
              ))}
            </CardContent>
          )}
        </Card>
      )}

      <div className="flex items-center justify-between">
        <Button variant="ghost" onClick={onBack}>
          Back
        </Button>
        <div className="flex items-center gap-3">
          <button
            onClick={() => {
              onChange({ serverUrl: "", headPreview: null, enabledLeafSlugs: [] });
              onComplete();
            }}
            className="text-sm text-muted-foreground hover:underline"
            disabled={isSubmitting}
          >
            Skip — I'll add one later
          </button>
          <Button onClick={onComplete} disabled={isSubmitting}>
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
    scheduleMode: "ALWAYS",
    idleThresholdMins: 5,
    scheduleStartHour: 22,
    scheduleEndHour: 8,
    serverUrl: "",
    headPreview: null,
    enabledLeafSlugs: [],
  });

  const update = (partial: Partial<WizardState>) => {
    setState((prev) => ({ ...prev, ...partial }));
  };

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

  const handleComplete = async () => {
    setIsSubmitting(true);
    setError(null);
    try {
      await invoke("run_init", {
        config: {
          cpu_cores: state.cpuCores,
          memory_mb: state.memoryMb,
          gpu_vram_pct: state.gpuVramPct,
          disk_gb: state.diskGb,
          schedule_mode: state.scheduleMode,
          idle_threshold_mins: state.idleThresholdMins,
          server_url: state.serverUrl || null,
          enabled_leafs:
            state.enabledLeafSlugs.length > 0 &&
            state.headPreview &&
            state.enabledLeafSlugs.length < state.headPreview.leafs.length
              ? state.enabledLeafSlugs
              : null,
        },
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
            onNext={() => setStep(5)}
            onBack={() => setStep(3)}
          />
        )}
        {step === 5 && (
          <ConnectStep
            state={state}
            onChange={update}
            onComplete={handleComplete}
            onBack={() => setStep(4)}
            isSubmitting={isSubmitting}
          />
        )}
      </div>
    </div>
  );
}
