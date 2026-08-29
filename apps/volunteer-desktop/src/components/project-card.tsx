import { useState } from "react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

interface VolunteerLeafCardProps {
  leafName: string;
  serverName: string;
  serverAddress: string;
  researchArea?: string;
  status: "active" | "paused" | "completed" | "error";
  creditEarned: number;
  workUnitsCompleted: number;
  onDetach?: () => void;
}

interface BrowseLeafCardProps {
  leafName: string;
  description: string;
  researchArea?: string;
  resourceRequirements: string;
  activeVolunteers: number;
  onAttach?: () => void;
  isAttaching?: boolean;
}

const statusConfig = {
  active: { label: "Active", color: "bg-green-500" },
  paused: { label: "Paused", color: "bg-yellow-500" },
  completed: { label: "Completed", color: "bg-blue-500" },
  error: { label: "Error", color: "bg-red-500" },
} as const;

export function VolunteerLeafCard({
  leafName,
  serverName,
  serverAddress,
  researchArea,
  status,
  creditEarned,
  workUnitsCompleted,
  onDetach,
}: VolunteerLeafCardProps) {
  const [confirmDetach, setConfirmDetach] = useState(false);
  const st = statusConfig[status];

  return (
    <Card>
      <CardHeader className="pb-3">
        <div className="flex items-start justify-between">
          <div className="space-y-1">
            <CardTitle className="text-base">{leafName}</CardTitle>
            <p className="text-xs text-muted-foreground">
              {serverName} · {serverAddress}
            </p>
          </div>
          <div className="flex items-center gap-1.5">
            <span className={cn("h-2 w-2 rounded-full", st.color)} />
            <span className="text-xs text-muted-foreground">{st.label}</span>
          </div>
        </div>
      </CardHeader>
      <CardContent className="space-y-3">
        {researchArea && (
          <span className="inline-block rounded-full bg-secondary px-2.5 py-0.5 text-xs font-medium">
            {researchArea}
          </span>
        )}
        <div className="flex justify-between text-sm">
          <span className="text-muted-foreground">Credit earned</span>
          <span className="font-medium">
            {creditEarned.toLocaleString()}
          </span>
        </div>
        <div className="flex justify-between text-sm">
          <span className="text-muted-foreground">Work units</span>
          <span className="font-medium">
            {workUnitsCompleted.toLocaleString()}
          </span>
        </div>

        {onDetach && !confirmDetach && (
          <Button
            variant="outline"
            size="sm"
            className="w-full"
            onClick={() => setConfirmDetach(true)}
          >
            Detach
          </Button>
        )}
        {onDetach && confirmDetach && (
          <div className="space-y-2 rounded-md border p-3">
            <p className="text-sm">
              Stop contributing to <strong>{leafName}</strong>?
            </p>
            <div className="flex gap-2">
              <Button
                variant="destructive"
                size="sm"
                className="flex-1"
                onClick={() => {
                  onDetach();
                  setConfirmDetach(false);
                }}
              >
                Detach
              </Button>
              <Button
                variant="outline"
                size="sm"
                className="flex-1"
                onClick={() => setConfirmDetach(false)}
              >
                Cancel
              </Button>
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  );
}

export function BrowseLeafCard({
  leafName,
  description,
  researchArea,
  resourceRequirements,
  activeVolunteers,
  onAttach,
  isAttaching,
}: BrowseLeafCardProps) {
  return (
    <Card>
      <CardHeader className="pb-3">
        <CardTitle className="text-base">{leafName}</CardTitle>
        {description && (
          <p className="text-sm text-muted-foreground line-clamp-2">
            {description}
          </p>
        )}
      </CardHeader>
      <CardContent className="space-y-3">
        <div className="flex flex-wrap gap-2">
          {researchArea && (
            <span className="inline-block rounded-full bg-secondary px-2.5 py-0.5 text-xs font-medium">
              {researchArea}
            </span>
          )}
          <span className="inline-block rounded-full bg-muted px-2.5 py-0.5 text-xs">
            {resourceRequirements}
          </span>
        </div>
        <div className="flex justify-between text-sm">
          <span className="text-muted-foreground">Active volunteers</span>
          <span className="font-medium">{activeVolunteers}</span>
        </div>
        {onAttach && (
          <Button
            size="sm"
            className="w-full"
            onClick={onAttach}
            disabled={isAttaching}
          >
            {isAttaching ? "Attaching..." : "Attach"}
          </Button>
        )}
      </CardContent>
    </Card>
  );
}
