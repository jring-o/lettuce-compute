"use client";

import { useEffect, useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { isWebGPUAvailable } from "@/lib/volunteer/webgpu-dispatch";

export interface HardwareInfo {
  cpuCores: number;
  memoryGB: number | null;
  webgpuAvailable: boolean;
  gpuAdapterName: string | null;
  wasmSupported: boolean;
  browser: string;
}

interface HardwarePanelProps {
  onDetected: (info: HardwareInfo) => void;
}

export function HardwarePanel({ onDetected }: HardwarePanelProps) {
  const [info, setInfo] = useState<HardwareInfo | null>(null);

  useEffect(() => {
    async function detect() {
      const cpuCores = navigator.hardwareConcurrency || 4;
      const memoryGB =
        "deviceMemory" in navigator
          ? (navigator as { deviceMemory?: number }).deviceMemory ?? null
          : null;

      let webgpuAvailable = false;
      let gpuAdapterName: string | null = null;
      try {
        webgpuAvailable = await isWebGPUAvailable();
        if (webgpuAvailable && navigator.gpu) {
          const adapter = await navigator.gpu.requestAdapter();
          if (adapter) {
            // adapter.info is available in modern browsers.
            const info = (adapter as unknown as { info?: { device?: string; description?: string } }).info;
            if (info) {
              gpuAdapterName = info.device || info.description || "Unknown GPU";
            }
          }
        }
      } catch {
        // WebGPU not available
      }

      const wasmSupported = typeof WebAssembly !== "undefined";
      const browser = navigator.userAgent;

      const detected: HardwareInfo = {
        cpuCores,
        memoryGB,
        webgpuAvailable,
        gpuAdapterName,
        wasmSupported,
        browser,
      };
      setInfo(detected);
      onDetected(detected);
    }
    detect();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  if (!info) {
    return (
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Hardware</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-sm text-muted-foreground">Detecting...</p>
        </CardContent>
      </Card>
    );
  }

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base">Hardware</CardTitle>
      </CardHeader>
      <CardContent>
        <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-sm">
          <dt className="text-muted-foreground">CPU Cores</dt>
          <dd>{info.cpuCores}</dd>

          <dt className="text-muted-foreground">Memory</dt>
          <dd>{info.memoryGB ? `${info.memoryGB} GB` : "Unknown"}</dd>

          <dt className="text-muted-foreground">WebGPU</dt>
          <dd>
            {info.webgpuAvailable ? (
              <Badge variant="default">
                Available{info.gpuAdapterName ? ` (${info.gpuAdapterName})` : ""}
              </Badge>
            ) : (
              <Badge variant="secondary">Not Available</Badge>
            )}
          </dd>

          <dt className="text-muted-foreground">WASM</dt>
          <dd>
            <Badge variant={info.wasmSupported ? "default" : "destructive"}>
              {info.wasmSupported ? "Supported" : "Not Supported"}
            </Badge>
          </dd>

          <dt className="text-muted-foreground">Browser</dt>
          <dd className="truncate" title={info.browser}>
            {summarizeBrowser(info.browser)}
          </dd>
        </dl>
      </CardContent>
    </Card>
  );
}

function summarizeBrowser(ua: string): string {
  if (ua.includes("Firefox/")) {
    const m = ua.match(/Firefox\/([\d.]+)/);
    return m ? `Firefox ${m[1]}` : "Firefox";
  }
  if (ua.includes("Edg/")) {
    const m = ua.match(/Edg\/([\d.]+)/);
    return m ? `Edge ${m[1]}` : "Edge";
  }
  if (ua.includes("Chrome/")) {
    const m = ua.match(/Chrome\/([\d.]+)/);
    return m ? `Chrome ${m[1]}` : "Chrome";
  }
  if (ua.includes("Safari/") && !ua.includes("Chrome")) {
    const m = ua.match(/Version\/([\d.]+)/);
    return m ? `Safari ${m[1]}` : "Safari";
  }
  return ua.slice(0, 40);
}
