import { useState, useEffect } from "react";
import { listen } from "@tauri-apps/api/event";
import { Activity, FolderKanban, Clock, Settings } from "lucide-react";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import { StatusBar } from "./status-bar";
import { UpdateBanner } from "@/components/update-banner";
import { OverviewPage } from "@/pages/overview";
import { ProjectsPage } from "@/pages/projects";
import { HistoryPage } from "@/pages/history";
import { SettingsPage } from "@/pages/settings";

const tabs = [
  { value: "overview", label: "Overview", icon: Activity },
  { value: "projects", label: "Projects", icon: FolderKanban },
  { value: "history", label: "History", icon: Clock },
  { value: "settings", label: "Settings", icon: Settings },
] as const;

export function TabLayout() {
  const [activeTab, setActiveTab] = useState("overview");

  useEffect(() => {
    const unlisten = listen("navigate:settings", () => {
      setActiveTab("settings");
    });
    return () => {
      unlisten.then((fn) => fn());
    };
  }, []);

  return (
    <div className="flex h-screen flex-col">
      <Tabs value={activeTab} onValueChange={setActiveTab} defaultValue="overview" className="flex flex-1 flex-col">
        <div className="border-b px-4">
          <TabsList className="w-full justify-start gap-1 bg-transparent">
            {tabs.map(({ value, label, icon: Icon }) => (
              <TabsTrigger key={value} value={value} className="gap-2">
                <Icon className="h-4 w-4" />
                {label}
              </TabsTrigger>
            ))}
          </TabsList>
        </div>

        <UpdateBanner />
        <div className="flex-1 overflow-auto">
          <TabsContent value="overview">
            <OverviewPage />
          </TabsContent>
          <TabsContent value="projects">
            <ProjectsPage />
          </TabsContent>
          <TabsContent value="history">
            <HistoryPage />
          </TabsContent>
          <TabsContent value="settings">
            <SettingsPage />
          </TabsContent>
        </div>
      </Tabs>

      <StatusBar />
    </div>
  );
}
