import { useState, useEffect, useRef, useLayoutEffect, useCallback } from "react";
import { listen } from "@tauri-apps/api/event";
import { Activity, FolderKanban, Clock, Settings } from "lucide-react";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "@/components/ui/tabs";
import { StatusBar } from "./status-bar";
import { UpdateBanner } from "@/components/update-banner";
import { RestartRequiredBanner } from "@/components/restart-required-banner";
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
  const activeTabRef = useRef(activeTab);

  // The page pane below is the app's only scroll container, and its offset
  // is remembered per tab so a return lands where the reader left off
  // (TB-68). The outgoing tab's offset must be captured before React swaps
  // the content: afterwards the pane has already clamped it to the new page.
  const paneRef = useRef<HTMLDivElement>(null);
  const scrollOffsets = useRef<Record<string, number>>({});

  const switchTab = useCallback((next: string) => {
    const current = activeTabRef.current;
    if (next === current) return;
    if (paneRef.current) scrollOffsets.current[current] = paneRef.current.scrollTop;
    setActiveTab(next);
  }, []);

  useLayoutEffect(() => {
    activeTabRef.current = activeTab;
    if (paneRef.current) paneRef.current.scrollTop = scrollOffsets.current[activeTab] ?? 0;
  }, [activeTab]);

  useEffect(() => {
    const unlisten = listen("navigate:settings", () => {
      switchTab("settings");
    });
    return () => {
      unlisten.then((fn) => fn());
    };
  }, [switchTab]);

  // `min-h-0` on the Tabs root: a flex item's minimum height defaults to its
  // content's, so without it the root grew to the page's full height, pushed
  // the status bar below the window and made the document scroll instead of
  // the page pane — the tab bar scrolled away with the page (TB-67).
  // `overflow-hidden` on the shell keeps anything else from doing the same.
  return (
    <div className="flex h-screen flex-col overflow-hidden">
      <Tabs
        value={activeTab}
        onValueChange={switchTab}
        defaultValue="overview"
        className="flex min-h-0 flex-1 flex-col"
      >
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
        <RestartRequiredBanner />
        <div ref={paneRef} data-testid="tab-pane" className="flex-1 overflow-auto">
          <TabsContent value="overview">
            <OverviewPage />
          </TabsContent>
          <TabsContent value="projects">
            <ProjectsPage />
          </TabsContent>
          {/* Kept mounted so the date range, the loaded rows and the scroll
              position survive a visit to another tab (TB-68). */}
          <TabsContent value="history" keepMounted>
            <HistoryPage active={activeTab === "history"} />
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
