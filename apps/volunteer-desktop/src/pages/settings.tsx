import { useState, useCallback, useEffect } from "react";
import { invoke } from "@tauri-apps/api/core";
import {
  ChevronDown,
  ChevronRight,
  Copy,
  Check,
  Monitor,
  Sun,
  Moon,
  RefreshCw,
} from "lucide-react";
import { useConfig } from "@/hooks/use-config";
import { restartLettuce, useOnDaemonRestart } from "@/hooks/use-restart-required";
import { useMetrics, useSystemMetrics } from "@/hooks/use-metrics";
import { useClient, useApiQuery } from "@/hooks/use-api";
import { MEMORY_SLIDER_STEP_MB, memorySliderMaxMb } from "@/lib/resource-limits";
import { ScheduleBuilder } from "@/components/schedule-builder";
import { ContainerRuntimeStatusCard } from "@/components/container-runtime-status";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Slider } from "@/components/ui/slider";
import { Card, CardContent } from "@/components/ui/card";
import {
  cn,
  formatBytes,
  formatGb,
  readStoredTheme,
  storeTheme,
  applyTheme,
  type Theme,
} from "@/lib/utils";
import {
  getDataDir,
  getSystemCpuCount,
  type ScheduleRange,
  type ThermalConfig,
  type ManagementClient,
} from "@/api/client";

// Collapsible section
function Section({
  title,
  defaultOpen = true,
  children,
}: {
  title: string;
  defaultOpen?: boolean;
  children: React.ReactNode;
}) {
  const [open, setOpen] = useState(defaultOpen);
  return (
    <div className="border rounded-lg">
      <button
        onClick={() => setOpen(!open)}
        className="flex items-center justify-between w-full px-4 py-3 text-sm font-medium hover:bg-muted/50 transition-colors"
      >
        {title}
        {open ? (
          <ChevronDown className="h-4 w-4" />
        ) : (
          <ChevronRight className="h-4 w-4" />
        )}
      </button>
      {open && <div className="px-4 pb-4 space-y-4">{children}</div>}
    </div>
  );
}

// Resource limit slider with usage bar
function ResourceSlider({
  label,
  value,
  min,
  max,
  step,
  displayValue,
  totalLabel,
  usagePct,
  disabled,
  onChange,
  logarithmic,
}: {
  label: string;
  value: number;
  min: number;
  max: number;
  step: number;
  displayValue: string;
  totalLabel?: string;
  usagePct?: number;
  disabled?: boolean;
  onChange: (v: number) => void;
  logarithmic?: boolean;
}) {
  // For logarithmic scale, map value to/from slider position
  const toSliderPos = logarithmic
    ? (v: number) => {
        if (v <= 0) return 0;
        return Math.log2(v);
      }
    : (v: number) => v;
  const fromSliderPos = logarithmic
    ? (p: number) => Math.round(Math.pow(2, p))
    : (p: number) => p;

  const sliderMin = logarithmic ? toSliderPos(min) : min;
  const sliderMax = logarithmic ? toSliderPos(max) : max;
  const sliderVal = toSliderPos(value);
  const sliderStep = logarithmic ? 0.5 : step;

  return (
    <div className="space-y-2">
      <div className="flex items-center justify-between">
        <span className="text-sm font-medium">{label}</span>
        <div className="text-sm text-right">
          <span className="font-medium">{displayValue}</span>
          {totalLabel && (
            <span className="text-muted-foreground ml-1">{totalLabel}</span>
          )}
        </div>
      </div>
      <Slider
        min={sliderMin}
        max={sliderMax}
        step={sliderStep}
        value={sliderVal}
        onChange={(pos) => onChange(fromSliderPos(pos))}
        disabled={disabled}
      />
      {usagePct !== undefined && (
        <div className="h-1 bg-secondary rounded-full overflow-hidden">
          <div
            className="h-full bg-muted-foreground/30 rounded-full transition-all"
            style={{ width: `${Math.min(100, usagePct)}%` }}
          />
        </div>
      )}
    </div>
  );
}

/**
 * A number input that saves when the user leaves the field or presses Enter,
 * so a value typed digit by digit is not written to the daemon on every
 * keystroke.
 */
function NumberField({
  label,
  value,
  min,
  max,
  step,
  suffix,
  disabled,
  onCommit,
}: {
  label: string;
  value: number;
  min?: number;
  max?: number;
  step?: number;
  suffix?: string;
  disabled?: boolean;
  onCommit: (v: number) => void;
}) {
  const [draft, setDraft] = useState(String(value));
  useEffect(() => {
    setDraft(String(value));
  }, [value]);

  const commit = () => {
    const parsed = Number(draft);
    if (!Number.isFinite(parsed)) {
      setDraft(String(value));
      return;
    }
    if (parsed !== value) onCommit(parsed);
  };

  return (
    <label className="flex items-center justify-between gap-3 text-sm">
      <span>{label}</span>
      <span className="flex items-center gap-1.5">
        <Input
          type="number"
          value={draft}
          min={min}
          max={max}
          step={step}
          disabled={disabled}
          aria-label={label}
          onChange={(e) => setDraft(e.target.value)}
          onBlur={commit}
          onKeyDown={(e) => {
            if (e.key === "Enter") (e.target as HTMLInputElement).blur();
          }}
          className="h-7 w-24 text-xs"
        />
        {suffix && <span className="text-xs text-muted-foreground w-8">{suffix}</span>}
      </span>
    </label>
  );
}

// Copy button
function CopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);

  const handleCopy = useCallback(() => {
    navigator.clipboard.writeText(text);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }, [text]);

  return (
    <Button variant="ghost" size="icon" className="h-8 w-8" onClick={handleCopy}>
      {copied ? (
        <Check className="h-3.5 w-3.5 text-green-500" />
      ) : (
        <Copy className="h-3.5 w-3.5" />
      )}
    </Button>
  );
}

// Toggle switch
function Toggle({
  checked,
  onChange,
  disabled,
  label,
}: {
  checked: boolean;
  onChange: (v: boolean) => void;
  disabled?: boolean;
  label?: string;
}) {
  return (
    <button
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={cn(
        "relative inline-flex h-5 w-9 items-center rounded-full transition-colors",
        checked ? "bg-primary" : "bg-secondary",
        disabled && "opacity-50 cursor-not-allowed"
      )}
    >
      <span
        className={cn(
          "inline-block h-4 w-4 rounded-full bg-white transition-transform",
          checked ? "translate-x-4.5" : "translate-x-0.5"
        )}
      />
    </button>
  );
}

/**
 * Restart the daemon from the General section. `restartLettuce()` stops the
 * running daemon (waiting up to 30 s, then forcing it), starts a fresh one,
 * and clears any pending "restart required" notice; in-progress work is
 * checkpointed by the daemon and resumed after the restart.
 */
function RestartButton() {
  const [restarting, setRestarting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);

  const handleRestart = async () => {
    setRestarting(true);
    setError(null);
    setDone(false);
    try {
      await restartLettuce();
      setDone(true);
      setTimeout(() => setDone(false), 4000);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setRestarting(false);
    }
  };

  return (
    <div className="space-y-1">
      <div className="flex items-center gap-3">
        <Button variant="outline" size="sm" onClick={handleRestart} disabled={restarting}>
          <RefreshCw className={cn("h-3.5 w-3.5 mr-1.5", restarting && "animate-spin")} />
          {restarting ? "Restarting Lettuce…" : "Restart Lettuce"}
        </Button>
        {restarting && (
          <span className="text-xs text-muted-foreground">
            Stopping the current daemon and starting a new one — this can take up to a minute.
          </span>
        )}
        {done && !restarting && (
          <span className="text-xs text-green-600">Lettuce restarted.</span>
        )}
      </div>
      {error && <p className="text-xs text-destructive">Restart failed: {error}</p>}
    </div>
  );
}

export function SettingsPage() {
  const { config, isLoading, updateConfig, toast, refetch } = useConfig();
  const { metrics } = useMetrics(5000);
  const { system } = useSystemMetrics(3000);
  const { data: headsResp, refetch: refetchHeads } = useApiQuery(
    (c: ManagementClient) => c.headsAndMachine(),
    60000
  );
  const machine = headsResp?.machine ?? null;
  const heads = headsResp?.heads ?? [];
  // The restarted daemon re-detects the machine and reconnects to heads
  // (`useConfig` refetches the config itself).
  useOnDaemonRestart(refetchHeads);

  const { client } = useClient();

  // Local state for settings that need immediate feedback
  const [confirmRegenerate, setConfirmRegenerate] = useState(false);
  const [regenerateError, setRegenerateError] = useState<string | null>(null);
  const [theme, setTheme] = useState<Theme>(() => readStoredTheme());
  const [autostart, setAutostart] = useState(true);

  // Verify identity state
  const [showVerifyDialog, setShowVerifyDialog] = useState(false);
  const [challengeHex, setChallengeHex] = useState("");
  const [signResult, setSignResult] = useState<{
    public_key: string;
    signature: string;
  } | null>(null);
  const [signError, setSignError] = useState<string | null>(null);
  const [signing, setSigning] = useState(false);

  // Sync autostart state from Tauri plugin
  useEffect(() => {
    invoke<boolean>("is_autostart_enabled")
      .then(setAutostart)
      .catch(() => {});
  }, []);

  // The data directory as the app resolves it (`LETTUCE_DATA_DIR` or
  // ~/.lettuce); the daemon's own `config.data_dir` is the fallback.
  const [hostDataDir, setHostDataDir] = useState<string | null>(null);
  useEffect(() => {
    getDataDir()
      .then((dir) => setHostDataDir(typeof dir === "string" && dir ? dir : null))
      .catch(() => {});
  }, []);

  // The machine's core count as the OS reports it to the Rust host. The web
  // view's `navigator.hardwareConcurrency` is capped at 8 by WebKit (Linux and
  // macOS), and this number is the hard CPU quota the daemon enforces, so a
  // browser-sized slider clamped a 256-thread host to 8 cores (TB-47). The
  // browser figure is only the fallback while detection is pending or failed.
  const [hostCpuCount, setHostCpuCount] = useState<number | null>(null);
  useEffect(() => {
    getSystemCpuCount()
      .then((n) => setHostCpuCount(typeof n === "number" && n > 0 ? n : null))
      .catch(() => {});
  }, []);

  const handleAutostartToggle = useCallback(async (enabled: boolean) => {
    setAutostart(enabled);
    try {
      await invoke("set_autostart", { enabled });
    } catch {
      // revert on failure
      setAutostart(!enabled);
    }
  }, []);

  // Update notification preference via config API
  const updateNotification = useCallback(
    (key: string, value: boolean | number) => {
      if (!config) return;
      updateConfig({
        notifications: { ...config.notifications, [key]: value },
      });
    },
    [config, updateConfig]
  );

  const updateThermal = useCallback(
    (patch: Partial<ThermalConfig>) => {
      if (!config) return;
      updateConfig({ thermal: { ...config.thermal, ...patch } });
    },
    [config, updateConfig]
  );

  // Apply the theme to the document and remember it for the next launch.
  useEffect(() => {
    storeTheme(theme);
    return applyTheme(theme);
  }, [theme]);

  if (isLoading || !config) {
    return (
      <div className="p-6 max-w-3xl mx-auto space-y-4">
        {Array.from({ length: 4 }).map((_, i) => (
          <div key={i} className="h-20 bg-muted rounded-lg animate-pulse" />
        ))}
      </div>
    );
  }

  // The host's count wins; the browser figure is a fallback, never a clamp: a
  // saved value above the known total (the CLI's default is NumCPU / 2) is
  // shown as it is rather than truncated the first time the slider is touched.
  const totalCores = hostCpuCount ?? navigator.hardwareConcurrency ?? 4;
  const coresSliderMax = Math.max(totalCores, config.resource_limits.max_cpu_cores);
  const tasksSliderMax = Math.max(totalCores, config.max_concurrent_tasks);
  const totalMemMB =
    system && system.memory_total_mb > 0 ? system.memory_total_mb : 8192;
  // 0 is the daemon's unit-count fallback; a daemon too old to report the
  // field reads as the daemon default (2 h).
  const workBufferHours = config.work_buffer_hours ?? 2;
  const hasGPU = machine?.has_gpu ?? false;
  const gpuPct = config.resource_limits.max_gpu_vram_pct;
  const gpuCardMb = machine?.gpu_card_vram_mb ?? 0;
  const gpuAllowedMb = Math.round((gpuCardMb * gpuPct) / 100);
  const thermal = config.thermal;
  const dataDir = hostDataDir ?? config.data_dir;

  const handleSignChallenge = async () => {
    if (!client || !challengeHex.trim()) return;
    setSigning(true);
    setSignError(null);
    setSignResult(null);
    try {
      const result = await client.signChallenge(challengeHex.trim());
      setSignResult(result);
    } catch (err) {
      setSignError(err instanceof Error ? err.message : "Failed to sign challenge");
    } finally {
      setSigning(false);
    }
  };

  return (
    <div className="p-6 max-w-3xl mx-auto space-y-4">
      {/* Toast */}
      {toast && (
        <div
          className={cn(
            "fixed top-4 right-4 z-50 rounded-md px-4 py-2 text-sm font-medium shadow-lg transition-all",
            toast.startsWith("Error")
              ? "bg-destructive text-destructive-foreground"
              : "bg-primary text-primary-foreground"
          )}
        >
          {toast}
        </div>
      )}

      {/* Section 1: Resource Limits */}
      <Section title="Resource Limits">
        <ResourceSlider
          label="CPU Cores"
          value={config.resource_limits.max_cpu_cores}
          min={1}
          max={coresSliderMax}
          step={1}
          displayValue={`${config.resource_limits.max_cpu_cores} / ${totalCores} cores`}
          usagePct={system?.cpu_usage_pct}
          onChange={(v) =>
            updateConfig({
              resource_limits: { ...config.resource_limits, max_cpu_cores: v },
            })
          }
        />

        <ResourceSlider
          label="Memory"
          value={config.resource_limits.max_memory_mb}
          min={MEMORY_SLIDER_STEP_MB}
          max={memorySliderMaxMb(totalMemMB, config.resource_limits.max_memory_mb)}
          step={MEMORY_SLIDER_STEP_MB}
          displayValue={`${formatBytes(config.resource_limits.max_memory_mb)} / ${formatBytes(totalMemMB)}`}
          usagePct={
            system && system.memory_total_mb > 0
              ? (system.memory_used_mb / system.memory_total_mb) * 100
              : undefined
          }
          onChange={(v) =>
            updateConfig({
              resource_limits: { ...config.resource_limits, max_memory_mb: v },
            })
          }
        />

        <ResourceSlider
          label="GPU allowance"
          value={gpuPct}
          min={0}
          max={100}
          step={5}
          displayValue={gpuPct === 0 ? "GPU disabled" : `${gpuPct}%`}
          disabled={!hasGPU}
          onChange={(v) =>
            updateConfig({
              resource_limits: {
                ...config.resource_limits,
                max_gpu_vram_pct: v,
              },
            })
          }
        />
        {hasGPU ? (
          <p className="text-xs text-muted-foreground">
            Leaves compare their VRAM need against this share of your card:
            {gpuCardMb > 0
              ? ` your card ${formatGb(gpuCardMb)} × ${gpuPct}% = ${formatGb(gpuAllowedMb)} allowed.`
              : ` ${gpuPct}% of your ${machine?.gpu_vendors[0] ?? ""} card.`}
            {gpuPct === 0 && " At 0% no GPU work is fetched."}
          </p>
        ) : (
          <p className="text-xs text-muted-foreground">No GPU detected</p>
        )}

        <ResourceSlider
          label="Disk Storage"
          value={config.resource_limits.max_disk_gb}
          min={1}
          max={100}
          step={1}
          displayValue={`${config.resource_limits.max_disk_gb} GB`}
          usagePct={
            metrics && metrics.disk_usage_known && metrics.disk_allowance_mb > 0
              ? (metrics.disk_used_mb / metrics.disk_allowance_mb) * 100
              : undefined
          }
          onChange={(v) =>
            updateConfig({
              resource_limits: { ...config.resource_limits, max_disk_gb: v },
            })
          }
        />
        <p className="text-xs text-muted-foreground">
          A cap on what Lettuce may use for work files and cached container images, not
          space it reserves. A leaf is fetched only when its declared need plus 2 GB of
          headroom fits inside this allowance.
          {metrics?.disk_usage_known &&
            ` Lettuce is using ${formatGb(metrics.disk_used_mb)} right now.`}
        </p>

        <ResourceSlider
          label="Network Bandwidth"
          value={
            config.resource_limits.max_bandwidth_mbps === 0
              ? 1024
              : config.resource_limits.max_bandwidth_mbps
          }
          min={1}
          max={1024}
          step={1}
          logarithmic
          displayValue={
            config.resource_limits.max_bandwidth_mbps === 0 ||
            config.resource_limits.max_bandwidth_mbps >= 1024
              ? "Unlimited"
              : `${config.resource_limits.max_bandwidth_mbps} Mbps`
          }
          onChange={(v) =>
            updateConfig({
              resource_limits: {
                ...config.resource_limits,
                max_bandwidth_mbps: v >= 1024 ? 0 : v,
              },
            })
          }
        />
      </Section>

      {/* Section 2: Compute */}
      <Section title="Compute">
        <ResourceSlider
          label="Concurrent Tasks"
          value={config.max_concurrent_tasks}
          min={1}
          max={tasksSliderMax}
          step={1}
          displayValue={`${config.max_concurrent_tasks}`}
          onChange={(v) => updateConfig({ max_concurrent_tasks: v })}
        />

        <ResourceSlider
          label="Work buffer"
          value={workBufferHours}
          min={0}
          max={12}
          step={0.5}
          displayValue={
            workBufferHours > 0
              ? `${workBufferHours} h of work per task`
              : "Fixed unit count (daemon fallback)"
          }
          onChange={(v) => updateConfig({ work_buffer_hours: v })}
        />
        <p className="text-xs text-muted-foreground">
          How many hours of work to keep fetched ahead for each running task, so
          computing continues if a head is slow or briefly unreachable. At 0 the
          daemon keeps a small fixed number of units instead. Applies straight away.
        </p>
      </Section>

      <Section title="Schedule">
        <ScheduleBuilder
          mode={config.scheduling.mode}
          idleThresholdMins={config.scheduling.idle_threshold_mins}
          scheduleRanges={config.scheduling.schedule_ranges}
          onModeChange={(mode) =>
            updateConfig({
              scheduling: { ...config.scheduling, mode },
            })
          }
          onIdleThresholdChange={(mins) =>
            updateConfig({
              scheduling: {
                ...config.scheduling,
                idle_threshold_mins: mins,
              },
            })
          }
          onScheduleChange={(ranges: ScheduleRange[]) =>
            updateConfig({
              scheduling: {
                ...config.scheduling,
                mode: "SCHEDULED",
                schedule_ranges: ranges,
              },
            })
          }
        />
      </Section>

      {/* Section 3: Thermal */}
      <Section title="Thermal" defaultOpen={false}>
        <div className="flex items-center justify-between">
          <div>
            <p className="text-sm font-medium">Pause when hot</p>
            <p className="text-xs text-muted-foreground">
              Freeze work when a sensor passes the pause temperature; continue once it
              cools below the resume temperature.
            </p>
          </div>
          <Toggle
            label="Pause when hot"
            checked={thermal.enabled}
            onChange={(v) => updateThermal({ enabled: v })}
          />
        </div>
        <div className="space-y-2">
          <NumberField
            label="CPU pause above"
            value={thermal.cpu_pause_threshold}
            min={40}
            max={110}
            suffix="°C"
            disabled={!thermal.enabled}
            onCommit={(v) => updateThermal({ cpu_pause_threshold: v })}
          />
          <NumberField
            label="CPU resume below"
            value={thermal.cpu_resume_threshold}
            min={30}
            max={110}
            suffix="°C"
            disabled={!thermal.enabled}
            onCommit={(v) => updateThermal({ cpu_resume_threshold: v })}
          />
          <NumberField
            label="GPU pause above"
            value={thermal.gpu_pause_threshold}
            min={40}
            max={110}
            suffix="°C"
            disabled={!thermal.enabled}
            onCommit={(v) => updateThermal({ gpu_pause_threshold: v })}
          />
          <NumberField
            label="GPU resume below"
            value={thermal.gpu_resume_threshold}
            min={30}
            max={110}
            suffix="°C"
            disabled={!thermal.enabled}
            onCommit={(v) => updateThermal({ gpu_resume_threshold: v })}
          />
          <NumberField
            label="Check every"
            value={thermal.poll_interval_seconds}
            min={1}
            max={600}
            suffix="s"
            disabled={!thermal.enabled}
            onCommit={(v) => updateThermal({ poll_interval_seconds: v })}
          />
          <NumberField
            label="Longest thermal pause"
            value={thermal.max_throttle_minutes}
            min={-1}
            max={1440}
            suffix="min"
            disabled={!thermal.enabled}
            onCommit={(v) => updateThermal({ max_throttle_minutes: v })}
          />
        </div>
        <p className="text-xs text-muted-foreground">
          The longest a thermal pause may hold before Lettuce releases it and checks the
          sensors again — a guard against a sensor that never reports cool. Work is never
          released while the sensor still reads above the resume temperature. 0 uses the
          default (30 minutes); -1 waits as long as it takes.
        </p>
      </Section>

      {/* Section 4: Container Runtime */}
      <Section title="Container Runtime">
        <ContainerRuntimeStatusCard />
      </Section>

      {/* Section 5: Identity */}
      <Section title="Identity" defaultOpen={false}>
        <div className="space-y-3">
          {/* Public Key */}
          {config.public_key && (
            <div className="space-y-1">
              <label className="text-xs font-medium text-muted-foreground">
                Public Key
              </label>
              <div className="flex items-center gap-2">
                <code className="flex-1 text-xs bg-muted rounded-md px-3 py-2 font-mono truncate">
                  {config.public_key}
                </code>
                <CopyButton text={config.public_key} />
              </div>
            </div>
          )}

          <div className="space-y-1">
            <label className="text-xs font-medium text-muted-foreground">
              Data directory
            </label>
            <div className="flex items-center gap-2">
              <code
                className="flex-1 text-xs bg-muted rounded-md px-3 py-2 font-mono truncate"
                title={dataDir}
              >
                {dataDir}
              </code>
              <CopyButton text={dataDir} />
            </div>
          </div>

          <div className="text-xs text-muted-foreground space-y-1">
            <p>
              Your keypair is your account. Every head you attach to recognises you by
              this public key, and credit from every machine using the same key is added
              together. One key can run on up to 10 machines.
            </p>
            <p>
              To move or copy your account to another computer, copy{" "}
              <code className="font-mono">identity.key</code> and{" "}
              <code className="font-mono">identity.pub</code> from the data directory
              above into the same folder there. Never re-run setup to fix a key problem —
              setup creates a new key, which is a new account with no credit.
            </p>
          </div>

          {heads.length > 0 && (
            <div className="space-y-1">
              <label className="text-xs font-medium text-muted-foreground">
                Volunteer IDs by head
              </label>
              <ul className="space-y-1">
                {heads.map((h) => (
                  <li key={h.name} className="flex items-center justify-between gap-3 text-xs">
                    <span className="font-medium">{h.name}</span>
                    {h.volunteer_id ? (
                      <span className="flex items-center gap-1">
                        <code className="font-mono bg-muted rounded px-2 py-0.5 truncate max-w-[16rem]">
                          {h.volunteer_id}
                        </code>
                        <CopyButton text={h.volunteer_id} />
                      </span>
                    ) : (
                      <span className="text-muted-foreground italic">not registered yet</span>
                    )}
                  </li>
                ))}
              </ul>
            </div>
          )}

          {/* Verify Identity */}
          <div className="pt-2 border-t space-y-2">
            {!showVerifyDialog ? (
              <Button
                variant="secondary"
                size="sm"
                onClick={() => {
                  setShowVerifyDialog(true);
                  setChallengeHex("");
                  setSignResult(null);
                  setSignError(null);
                }}
              >
                Verify Identity
              </Button>
            ) : (
              <Card>
                <CardContent className="p-4 space-y-3">
                  <p className="text-sm text-muted-foreground">
                    An external verifier will give you a challenge code. Paste it
                    below to sign with your private key.
                  </p>
                  <div className="space-y-2">
                    <Input
                      value={challengeHex}
                      onChange={(e) => setChallengeHex(e.target.value)}
                      placeholder="Paste challenge hex here"
                      className="font-mono text-xs"
                    />
                    <div className="flex gap-2">
                      <Button
                        size="sm"
                        onClick={handleSignChallenge}
                        disabled={signing || !challengeHex.trim()}
                      >
                        {signing ? "Signing..." : "Sign"}
                      </Button>
                      <Button
                        variant="outline"
                        size="sm"
                        onClick={() => setShowVerifyDialog(false)}
                      >
                        Cancel
                      </Button>
                    </div>
                  </div>

                  {signError && (
                    <p className="text-xs text-destructive">{signError}</p>
                  )}

                  {signResult && (
                    <div className="space-y-2">
                      <div className="space-y-1">
                        <label className="text-xs font-medium text-muted-foreground">
                          Public Key
                        </label>
                        <div className="flex items-center gap-2">
                          <code className="flex-1 text-xs bg-muted rounded-md px-3 py-2 font-mono truncate">
                            {signResult.public_key}
                          </code>
                          <CopyButton text={signResult.public_key} />
                        </div>
                      </div>
                      <div className="space-y-1">
                        <label className="text-xs font-medium text-muted-foreground">
                          Signature
                        </label>
                        <div className="flex items-center gap-2">
                          <code className="flex-1 text-xs bg-muted rounded-md px-3 py-2 font-mono truncate">
                            {signResult.signature}
                          </code>
                          <CopyButton text={signResult.signature} />
                        </div>
                      </div>
                    </div>
                  )}
                </CardContent>
              </Card>
            )}
          </div>

          {/* Regenerate Keypair */}
          <div className="pt-2 border-t">
            {!confirmRegenerate ? (
              <Button
                variant="destructive"
                size="sm"
                onClick={() => setConfirmRegenerate(true)}
              >
                Regenerate Keypair
              </Button>
            ) : (
              <Card className="border-destructive">
                <CardContent className="p-4 space-y-3">
                  <p className="text-sm">
                    This will generate a new identity — a new account. Credit earned
                    under the current key stays with that key on every head and does not
                    transfer. Are you sure?
                  </p>
                  <div className="flex gap-2">
                    <Button
                      variant="destructive"
                      size="sm"
                      onClick={async () => {
                        setRegenerateError(null);
                        try {
                          await invoke<string>("regenerate_keypair");
                          refetch();
                          setConfirmRegenerate(false);
                        } catch (err) {
                          setRegenerateError(
                            err instanceof Error ? err.message : "Failed to regenerate keypair"
                          );
                        }
                      }}
                    >
                      Yes, Regenerate
                    </Button>
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => setConfirmRegenerate(false)}
                    >
                      Cancel
                    </Button>
                  </div>
                  {regenerateError && (
                    <p className="text-xs text-destructive">{regenerateError}</p>
                  )}
                </CardContent>
              </Card>
            )}
          </div>
        </div>
      </Section>

      {/* Section 6: General */}
      <Section title="General" defaultOpen={false}>
        <div className="space-y-4">
          {/* Theme */}
          <div className="space-y-2">
            <label className="text-sm font-medium">Theme</label>
            <div className="flex gap-1 rounded-lg bg-muted p-1">
              {[
                { value: "system" as const, label: "System", icon: Monitor },
                { value: "light" as const, label: "Light", icon: Sun },
                { value: "dark" as const, label: "Dark", icon: Moon },
              ].map(({ value, label, icon: Icon }) => (
                <button
                  key={value}
                  onClick={() => setTheme(value)}
                  className={cn(
                    "flex-1 flex items-center justify-center gap-1.5 rounded-md px-3 py-1.5 text-sm font-medium transition-colors",
                    theme === value
                      ? "bg-background text-foreground shadow-sm"
                      : "text-muted-foreground hover:text-foreground"
                  )}
                >
                  <Icon className="h-3.5 w-3.5" />
                  {label}
                </button>
              ))}
            </div>
          </div>

          {/* Auto-Start */}
          <div className="flex items-center justify-between">
            <div>
              <p className="text-sm font-medium">Start on boot</p>
              <p className="text-xs text-muted-foreground">
                Launch minimized to system tray when you log in
              </p>
            </div>
            <Toggle
              label="Start on boot"
              checked={autostart}
              onChange={handleAutostartToggle}
            />
          </div>

          {/* Notifications */}
          <div className="space-y-3">
            <label className="text-sm font-medium">Notifications</label>

            <div className="space-y-2">
              <div className="flex items-center justify-between">
                <span className="text-sm">Credit milestones</span>
                <div className="flex items-center gap-2">
                  <Input
                    type="number"
                    value={config.notifications.credit_milestone_threshold}
                    onChange={(e) =>
                      updateNotification(
                        "credit_milestone_threshold",
                        parseInt(e.target.value) || 100
                      )
                    }
                    className="h-7 w-20 text-xs"
                    min={1}
                  />
                  <Toggle
                    checked={config.notifications.credit_milestones}
                    onChange={(v) =>
                      updateNotification("credit_milestones", v)
                    }
                  />
                </div>
              </div>

              <div className="flex items-center justify-between">
                <span className="text-sm">Errors requiring attention</span>
                <Toggle
                  checked={config.notifications.errors}
                  onChange={(v) =>
                    updateNotification("errors", v)
                  }
                />
              </div>

              <div className="flex items-center justify-between">
                <span className="text-sm">Work unit completed</span>
                <Toggle
                  checked={config.notifications.work_unit_completed}
                  onChange={(v) =>
                    updateNotification("work_unit_completed", v)
                  }
                />
              </div>

              <div className="flex items-center justify-between">
                <span className="text-sm">Update available</span>
                <Toggle
                  checked={config.notifications.updates}
                  onChange={(v) =>
                    updateNotification("updates", v)
                  }
                />
              </div>
            </div>
          </div>

          {/* Log Level */}
          <div className="space-y-2">
            <label className="text-sm font-medium">Log Level</label>
            <select
              value={config.log_level}
              onChange={(e) => updateConfig({ log_level: e.target.value })}
              className="h-9 w-full rounded-md border border-input bg-background px-3 text-sm"
            >
              <option value="error">Error</option>
              <option value="warn">Warn</option>
              <option value="info">Info</option>
              <option value="debug">Debug</option>
            </select>
          </div>

          {/* Restart */}
          <div className="space-y-2 pt-2 border-t">
            <p className="text-sm font-medium">Lettuce service</p>
            <p className="text-xs text-muted-foreground">
              Stops the background daemon and starts it again. Running work is
              checkpointed and picked up after the restart.
            </p>
            <RestartButton />
          </div>
        </div>
      </Section>
    </div>
  );
}
