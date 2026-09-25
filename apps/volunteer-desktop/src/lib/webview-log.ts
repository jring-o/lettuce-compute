import { invoke } from "@tauri-apps/api/core";

type Level = "error" | "warn";

/** Longest message sent per line; the host cuts longer ones as well. */
const MAX_MESSAGE_CHARS = 4000;

function describe(value: unknown): string {
  if (value instanceof Error) return value.stack || `${value.name}: ${value.message}`;
  if (typeof value === "string") return value;
  try {
    return JSON.stringify(value) ?? String(value);
  } catch {
    return String(value);
  }
}

function send(level: Level, message: string): void {
  invoke("log_from_webview", { level, message: message.slice(0, MAX_MESSAGE_CHARS) }).catch(() => {
    // The host is where the log is written; if it cannot be reached there is
    // nowhere else to put the line, and reporting that to the console would
    // only come back here.
  });
}

/**
 * Forward the web view's own problems to the app log (`desktop.log`, through
 * the host's `log_from_webview` command): console errors and warnings,
 * uncaught errors (React reports an uncaught render error this way too) and
 * unhandled promise rejections. Without this they reach only the web view's
 * console, which is never saved. The console keeps printing everything as
 * before. Returns a function that undoes the forwarding.
 */
export function forwardWebviewLogs(): () => void {
  const originalError = console.error;
  const originalWarn = console.warn;

  console.error = (...args: unknown[]) => {
    originalError.apply(console, args);
    send("error", args.map(describe).join(" "));
  };
  console.warn = (...args: unknown[]) => {
    originalWarn.apply(console, args);
    send("warn", args.map(describe).join(" "));
  };

  const onError = (event: ErrorEvent) => {
    const where = event.filename ? ` (${event.filename}:${event.lineno}:${event.colno})` : "";
    send("error", `uncaught error: ${describe(event.error ?? event.message)}${where}`);
  };
  const onRejection = (event: PromiseRejectionEvent) => {
    send("error", `unhandled promise rejection: ${describe(event.reason)}`);
  };
  window.addEventListener("error", onError);
  window.addEventListener("unhandledrejection", onRejection);

  return () => {
    console.error = originalError;
    console.warn = originalWarn;
    window.removeEventListener("error", onError);
    window.removeEventListener("unhandledrejection", onRejection);
  };
}
