import { useState } from "react";
import { useContainerRuntime } from "@/hooks/use-container-runtime";
import {
  setupContainerRuntime,
  startContainerRuntime,
  stopContainerRuntime,
  redetectContainerRuntime,
  installPodman,
} from "@/api/client";
import { Button } from "@/components/ui/button";
import { cn, detectPlatform } from "@/lib/utils";

const platform = detectPlatform();

function StatusDot({ color }: { color: "green" | "yellow" | "gray" | "red" }) {
  const colors = {
    green: "bg-green-500",
    yellow: "bg-yellow-500",
    gray: "bg-gray-400",
    red: "bg-red-500",
  };
  return <span className={cn("inline-block h-2.5 w-2.5 rounded-full shrink-0", colors[color])} />;
}

export function ContainerRuntimeStatusCard() {
  const { status, loading, error, refresh } = useContainerRuntime();
  const [actionLoading, setActionLoading] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  const handleAction = async (action: () => Promise<unknown>) => {
    setActionLoading(true);
    setActionError(null);
    try {
      await action();
      await refresh();
    } catch (err) {
      setActionError(err instanceof Error ? err.message : String(err));
    } finally {
      setActionLoading(false);
    }
  };

  if (loading && !status) {
    return (
      <div className="h-20 bg-muted rounded-lg animate-pulse" />
    );
  }

  if (error && !status) {
    return (
      <div className="space-y-2">
        <div className="flex items-center gap-2">
          <StatusDot color="red" />
          <span className="text-sm font-medium">Container Runtime</span>
        </div>
        <p className="text-xs text-destructive">{error}</p>
        <Button variant="outline" size="sm" onClick={refresh}>
          Retry
        </Button>
      </div>
    );
  }

  if (!status) return null;

  // Running — Podman
  if (status.status === "running" && status.backend === "podman") {
    return (
      <div className="space-y-3">
        <div className="flex items-center gap-2">
          <StatusDot color="green" />
          <span className="text-sm font-medium">Podman {status.version}</span>
        </div>
        {status.machine_required && (
          <div className="text-xs text-muted-foreground space-y-0.5">
            <p>Machine: {status.machine_name || "default"}</p>
            <p>
              Resources: {status.machine_cpus} CPUs, {Math.round(status.machine_memory_mb / 1024)} GB RAM, {status.machine_disk_gb} GB disk
            </p>
          </div>
        )}
        {status.machine_required && (
          <Button
            variant="outline"
            size="sm"
            disabled={actionLoading}
            onClick={() => handleAction(stopContainerRuntime)}
          >
            {actionLoading ? "Stopping..." : "Stop Machine"}
          </Button>
        )}
        {actionError && <p className="text-xs text-destructive">{actionError}</p>}
      </div>
    );
  }

  // Running — Podman answering the Docker-compatible socket (Podman Desktop's
  // Docker compatibility, podman-mac-helper). The probe that found it was the
  // Docker one, so the card used to call it Docker and advise installing the
  // Podman that was already running (TB-73). Lettuce does not manage the
  // machine on this path, so there are no machine figures or buttons here.
  if (status.status === "running" && status.backend === "docker" && status.engine === "podman") {
    return (
      <div className="space-y-3">
        <div className="flex items-center gap-2">
          <StatusDot color="green" />
          <span className="text-sm font-medium">
            Podman{status.version ? ` ${status.version}` : ""}
          </span>
        </div>
        <p className="text-xs text-muted-foreground">
          Reached through the Docker-compatible socket, so this app does not manage the Podman
          machine: start, stop and resize it from Podman Desktop or the podman command. To have
          Lettuce manage the machine instead, run{" "}
          <code>lettuce-volunteer config set container_backend podman</code> and restart the app.
        </p>
      </div>
    );
  }

  // Running — Docker
  if (status.status === "running" && status.backend === "docker") {
    return (
      <div className="space-y-3">
        <div className="flex items-center gap-2">
          <StatusDot color="green" />
          <span className="text-sm font-medium">Docker {status.version}</span>
        </div>
        <p className="text-xs text-muted-foreground">
          Using system Docker. Install Podman for a lighter alternative.
        </p>
      </div>
    );
  }

  // Stopped (Podman machine)
  if (status.status === "stopped") {
    return (
      <div className="space-y-3">
        <div className="flex items-center gap-2">
          <StatusDot color="yellow" />
          <span className="text-sm font-medium">Machine stopped</span>
        </div>
        <p className="text-xs text-muted-foreground">
          Container leafs unavailable until machine is started.
        </p>
        <Button
          variant="outline"
          size="sm"
          disabled={actionLoading}
          onClick={() => handleAction(startContainerRuntime)}
        >
          {actionLoading ? "Starting..." : "Start Machine"}
        </Button>
        {actionError && <p className="text-xs text-destructive">{actionError}</p>}
      </div>
    );
  }

  // Starting
  if (status.status === "starting") {
    return (
      <div className="space-y-3">
        <div className="flex items-center gap-2">
          <StatusDot color="yellow" />
          <span className="text-sm font-medium">Starting...</span>
        </div>
        <p className="text-xs text-muted-foreground">
          Container runtime is starting up. This may take a moment.
        </p>
      </div>
    );
  }

  // Not Initialized (Podman installed, machine not init)
  if (status.status === "not_initialized") {
    return (
      <div className="space-y-3">
        <div className="flex items-center gap-2">
          <StatusDot color="yellow" />
          <span className="text-sm font-medium">Setup Required</span>
        </div>
        <p className="text-xs text-muted-foreground">
          Podman is installed but needs initial setup.
        </p>
        <Button
          variant="outline"
          size="sm"
          disabled={actionLoading}
          onClick={() => handleAction(setupContainerRuntime)}
        >
          {actionLoading ? "Setting up..." : "Setup Container Runtime"}
        </Button>
        {actionError && <p className="text-xs text-destructive">{actionError}</p>}
      </div>
    );
  }

  // Not Installed
  if (status.status === "not_installed") {
    return (
      <div className="space-y-3">
        <div className="flex items-center gap-2">
          <StatusDot color="gray" />
          <span className="text-sm font-medium">No container runtime installed</span>
        </div>
        <p className="text-xs text-muted-foreground">
          {platform === "linux" &&
            "Install Podman from your distribution's packages, then start its API socket as your normal user: systemctl --user enable --now podman.socket. Docker also works when your user can reach its socket."}
          {platform === "windows" &&
            "Podman can be installed automatically from the installer bundled with this app (WSL2 required)."}
          {platform === "macos" &&
            "Install Podman Desktop or Docker Desktop and start it; its machine (a small Linux VM) must be running."}
        </p>
        {platform === "windows" && (
          <Button
            variant="outline"
            size="sm"
            disabled={actionLoading}
            onClick={() => handleAction(() => installPodman())}
          >
            {actionLoading ? "Installing..." : "Install Podman"}
          </Button>
        )}
        <p className="text-xs text-muted-foreground">
          {status.redetecting
            ? "Lettuce checks for an engine every minute and starts container work as soon as one answers, no restart needed. WASM and native leafs run without one."
            : "Container leafs will be unavailable until a runtime is installed. WASM and native leafs run without one."}
        </p>
        {status.redetecting && (
          <Button
            variant="outline"
            size="sm"
            disabled={actionLoading}
            onClick={() => handleAction(redetectContainerRuntime)}
          >
            {actionLoading ? "Checking..." : "Check again now"}
          </Button>
        )}
        {actionError && <p className="text-xs text-destructive">{actionError}</p>}
      </div>
    );
  }

  // Error
  if (status.status === "error") {
    return (
      <div className="space-y-3">
        <div className="flex items-center gap-2">
          <StatusDot color="red" />
          <span className="text-sm font-medium">Container Runtime Error</span>
        </div>
        <p className="text-xs text-destructive">{status.error}</p>
        <Button variant="outline" size="sm" onClick={refresh}>
          Retry
        </Button>
      </div>
    );
  }

  return null;
}
