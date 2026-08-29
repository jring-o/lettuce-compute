import { AlertTriangle, OctagonAlert, X } from "lucide-react";
import { useNotices } from "@/hooks/use-notices";
import { cn, formatTimeAgo } from "@/lib/utils";
import type { Notice } from "@/api/client";

function NoticeRow({ notice, onDismiss }: { notice: Notice; onDismiss: (n: Notice) => void }) {
  const isError = notice.level === "error";
  const Icon = isError ? OctagonAlert : AlertTriangle;
  const scope = [notice.head, notice.leaf].filter(Boolean).join(" / ");

  return (
    <li
      className={cn(
        "flex items-start gap-3 rounded-md border px-3 py-2 text-sm",
        isError
          ? "border-red-500/30 bg-red-500/5"
          : "border-yellow-500/30 bg-yellow-500/5"
      )}
      data-testid={`notice-${notice.id}`}
    >
      <Icon
        aria-label={isError ? "Error" : "Warning"}
        className={cn("mt-0.5 h-4 w-4 shrink-0", isError ? "text-red-500" : "text-yellow-600")}
      />
      <div className="min-w-0 flex-1 space-y-0.5">
        <p className="break-words">{notice.message}</p>
        <p className="text-xs text-muted-foreground">
          {scope && <span>{scope} · </span>}
          {notice.count > 1 && <span>{notice.count} times · </span>}
          <span>{formatTimeAgo(notice.at)}</span>
        </p>
      </div>
      <button
        type="button"
        onClick={() => onDismiss(notice)}
        aria-label="Dismiss notice"
        className="shrink-0 rounded p-1 text-muted-foreground hover:bg-accent hover:text-foreground"
      >
        <X className="h-3.5 w-3.5" />
      </button>
    </li>
  );
}

/**
 * Warnings and errors the daemon has raised (`GET /api/v1/notices`), newest
 * first. Renders nothing when there is nothing to show, or when the running
 * CLI build does not provide the route.
 */
export function NoticesPanel() {
  const { notices, supported, dismiss } = useNotices(10000);
  if (!supported || notices.length === 0) return null;

  return (
    <section className="space-y-2" aria-label="Notices">
      <h2 className="text-sm font-medium text-muted-foreground">Needs attention</h2>
      <ul className="space-y-2">
        {notices.map((n) => (
          <NoticeRow key={n.id} notice={n} onDismiss={dismiss} />
        ))}
      </ul>
    </section>
  );
}
