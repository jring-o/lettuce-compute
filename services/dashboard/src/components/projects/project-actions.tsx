"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import {
  Pause,
  Play,
  Archive,
  Download,
} from "lucide-react";
import {
  pauseLeaf,
  resumeLeaf,
  archiveLeaf,
} from "@/lib/actions/projects";
import type { LeafState } from "@/types/infrastructure";
import { Button, buttonVariants } from "@/components/ui/button";

interface ProjectActionsProps {
  leafId: string;
  leafState: LeafState;
  leafSlug: string;
  hasCompletedWorkUnits: boolean;
}

export function ProjectActions({
  leafId,
  leafState,
  leafSlug,
  hasCompletedWorkUnits,
}: ProjectActionsProps) {
  const router = useRouter();
  const [loading, setLoading] = useState<string | null>(null);

  const handleAction = async (
    action: "pause" | "resume" | "archive",
  ) => {
    if (action === "pause") {
      if (!window.confirm("Pause this leaf? Volunteers will stop receiving new work units.")) return;
    }
    if (action === "archive") {
      if (!window.confirm("Archive this leaf? This cannot be undone.")) return;
    }

    setLoading(action);
    try {
      const actionFn =
        action === "pause"
          ? pauseLeaf
          : action === "resume"
            ? resumeLeaf
            : archiveLeaf;

      const result = await actionFn(leafId);
      if ("error" in result) {
        alert(result.error.message);
      } else {
        router.refresh();
      }
    } finally {
      setLoading(null);
    }
  };

  return (
    <div data-testid="project-actions" className="flex items-center gap-2">
      {leafState === "ACTIVE" && (
        <Button
          variant="outline"
          size="sm"
          onClick={() => handleAction("pause")}
          disabled={loading !== null}
          data-testid="pause-button"
        >
          <Pause className="size-3.5" />
          {loading === "pause" ? "Pausing..." : "Pause"}
        </Button>
      )}

      {leafState === "PAUSED" && (
        <Button
          variant="outline"
          size="sm"
          onClick={() => handleAction("resume")}
          disabled={loading !== null}
          data-testid="resume-button"
        >
          <Play className="size-3.5" />
          {loading === "resume" ? "Resuming..." : "Resume"}
        </Button>
      )}

      {(leafState === "PAUSED" || leafState === "COMPLETED") && (
        <Button
          variant="destructive"
          size="sm"
          onClick={() => handleAction("archive")}
          disabled={loading !== null}
          data-testid="archive-button"
        >
          <Archive className="size-3.5" />
          {loading === "archive" ? "Archiving..." : "Archive"}
        </Button>
      )}

      {hasCompletedWorkUnits && (
        <a
          href={`/api/download/${leafId}?format=json`}
          download
          data-testid="download-button"
          className={buttonVariants({ variant: "outline", size: "sm" })}
        >
          <Download className="size-3.5" />
          Download Results
        </a>
      )}
    </div>
  );
}
