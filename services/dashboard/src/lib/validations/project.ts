// --- Supported Platforms ---

export const SUPPORTED_PLATFORMS = [
  { key: "linux_amd64", label: "Linux (x86_64)" },
  { key: "linux_arm64", label: "Linux (ARM64)" },
  { key: "darwin_amd64", label: "macOS (Intel)" },
  { key: "darwin_arm64", label: "macOS (Apple Silicon)" },
  { key: "windows_amd64", label: "Windows (x86_64)" },
] as const;

// --- Research Area ---

export interface ResearchArea {
  id: string;
  slug: string;
  name: string;
  description: string | null;
}
