import { useEffect, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import { TabLayout } from "@/components/layout/tab-layout";
import { SetupWizard } from "@/components/wizard/setup-wizard";
import lettuceLeaf from "@/assets/lettuce-leaf.png";

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

  const checkInit = async () => {
    try {
      const initialized = await invoke<boolean>("is_initialized");
      setIsInitialized(initialized);
    } catch {
      setIsInitialized(false);
    } finally {
      setIsLoading(false);
    }
  };

  useEffect(() => {
    checkInit();
  }, []);

  if (isLoading) return <LoadingScreen />;
  if (!isInitialized) return <SetupWizard onComplete={checkInit} />;
  return <TabLayout />;
}
