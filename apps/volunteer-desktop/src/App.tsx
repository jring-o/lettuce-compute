import { useEffect, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import { emit } from "@tauri-apps/api/event";
import { TabLayout } from "@/components/layout/tab-layout";
import { SetupWizard } from "@/components/wizard/setup-wizard";
import { applyTheme, readStoredTheme } from "@/lib/utils";
import lettuceLeaf from "@/assets/lettuce-leaf.png";

/**
 * Emitted once the setup wizard has written the install's config, before
 * the daemon is up. The Rust host listens for it (`APP_INITIALIZED_EVENT` in
 * `main.rs`) to start the tray poll, notifications and the container runtime
 * without the app being relaunched. It must not wait for the daemon: when
 * the first start was slow or failed, waiting left those services unstarted
 * for the whole session and the tray at "Stopped" (TB-55).
 */
const APP_INITIALIZED_EVENT = "app_initialized";

function LoadingScreen() {
  return (
    <div className="flex min-h-screen items-center justify-center">
      <div className="text-center space-y-4">
        <img src={lettuceLeaf} alt="Lettuce" className="w-48 rounded-lg" />
        <p className="text-muted-foreground">Starting Lettuce Compute...</p>
      </div>
    </div>
  );
}

export function App() {
  const [isLoading, setIsLoading] = useState(true);
  const [isInitialized, setIsInitialized] = useState(false);

  // Apply the saved theme before any page renders, so the window does not
  // flash the wrong colours until Settings is opened.
  useEffect(() => applyTheme(readStoredTheme()), []);

  const checkInit = async (): Promise<boolean> => {
    let initialized = false;
    try {
      initialized = await invoke<boolean>("is_initialized");
    } catch {
      initialized = false;
    }
    setIsInitialized(initialized);
    setIsLoading(false);
    return initialized;
  };

  useEffect(() => {
    checkInit();
  }, []);

  const handleWizardInitialized = async () => {
    try {
      await emit(APP_INITIALIZED_EVENT);
    } catch {
      // Not fatal: the host also starts its services on the next launch.
    }
  };

  const handleWizardComplete = async () => {
    await checkInit();
  };

  if (isLoading) return <LoadingScreen />;
  if (!isInitialized) {
    return (
      <SetupWizard
        onInitialized={handleWizardInitialized}
        onComplete={handleWizardComplete}
      />
    );
  }
  return <TabLayout />;
}
