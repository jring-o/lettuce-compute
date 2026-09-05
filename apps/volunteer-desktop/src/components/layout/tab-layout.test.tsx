import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { listen } from "@tauri-apps/api/event";

vi.mock("@/pages/overview", () => ({ OverviewPage: () => <p>overview page</p> }));
vi.mock("@/pages/projects", () => ({ ProjectsPage: () => <p>projects page</p> }));
vi.mock("@/pages/settings", () => ({ SettingsPage: () => <p>settings page</p> }));

// The History page stands in for itself with one piece of local state, and
// records the `active` prop the layout gives it.
const historyActive = vi.fn();
vi.mock("@/pages/history", () => ({
  HistoryPage: ({ active }: { active?: boolean }) => {
    historyActive(active);
    const [count, setCount] = useState(0);
    return (
      <div>
        <p>history page</p>
        <button onClick={() => setCount((c) => c + 1)}>count {count}</button>
      </div>
    );
  },
}));

vi.mock("./status-bar", () => ({ StatusBar: () => <footer>status bar</footer> }));
vi.mock("@/components/update-banner", () => ({ UpdateBanner: () => null }));
vi.mock("@/components/restart-required-banner", () => ({
  RestartRequiredBanner: () => null,
}));

import { TabLayout } from "./tab-layout";

/** The page pane: the element holding the tab panels. */
function pagePane(): HTMLElement {
  return screen.getByRole("tabpanel").parentElement!;
}

describe("TabLayout", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.mocked(listen).mockImplementation(() => Promise.resolve(() => {}));
  });

  // TB-67: the shell is the window's height, and the page pane inside it is
  // meant to be the only thing that scrolls, with the tab bar above and the
  // status bar below pinned. A flex item's minimum height is its content's
  // unless told otherwise, so every flex item between the shell and the pane
  // needs `min-h-0` — without it the Tabs root grew to the page's height,
  // pushed the status bar below the window and the document scrolled.
  it("TB-67: the shell clips, and every flex item between it and the page pane has min-h-0", () => {
    const { container } = render(<TabLayout />);
    const shell = container.firstElementChild as HTMLElement;
    const pane = pagePane();
    expect(shell).toHaveClass("h-screen", "flex", "flex-col", "overflow-hidden");
    expect(pane).toHaveClass("flex-1", "overflow-auto");

    const between: HTMLElement[] = [];
    for (let el = pane.parentElement; el && el !== shell; el = el.parentElement) {
      between.push(el);
    }
    expect(between.length).toBeGreaterThan(0);
    for (const el of between) {
      expect(el).toHaveClass("flex", "flex-col", "flex-1", "min-h-0");
    }
  });

  it("TB-68: the History page stays mounted while another tab is shown, and is told when it is shown again", async () => {
    const user = userEvent.setup();
    render(<TabLayout />);

    await user.click(screen.getByRole("tab", { name: "History" }));
    await user.click(screen.getByText("count 0"));
    expect(screen.getByText("count 1")).toBeVisible();
    expect(historyActive).toHaveBeenLastCalledWith(true);

    await user.click(screen.getByRole("tab", { name: "Overview" }));
    expect(screen.getByText("overview page")).toBeVisible();
    // Still mounted, hidden: the state it holds is not lost.
    expect(screen.getByText("count 1")).toBeInTheDocument();
    expect(screen.getByText("count 1")).not.toBeVisible();
    expect(historyActive).toHaveBeenLastCalledWith(false);

    await user.click(screen.getByRole("tab", { name: "History" }));
    expect(screen.getByText("count 1")).toBeVisible();
    expect(historyActive).toHaveBeenLastCalledWith(true);
    // The other pages keep the old behavior: unmounted while not shown.
    expect(screen.queryByText("overview page")).not.toBeInTheDocument();
  });

  it("TB-68: the page pane's scroll offset is remembered per tab", async () => {
    const user = userEvent.setup();
    render(<TabLayout />);
    const pane = pagePane();
    // jsdom does no layout, so the offset is a plain stored value here.
    let scrollTop = 0;
    Object.defineProperty(pane, "scrollTop", {
      get: () => scrollTop,
      set: (v: number) => {
        scrollTop = v;
      },
      configurable: true,
    });

    await user.click(screen.getByRole("tab", { name: "History" }));
    scrollTop = 1234;
    await user.click(screen.getByRole("tab", { name: "Overview" }));
    expect(scrollTop).toBe(0);
    await user.click(screen.getByRole("tab", { name: "History" }));
    expect(scrollTop).toBe(1234);
  });

  it("switches to Settings when the host asks for it", () => {
    let onNavigate: (() => void) | undefined;
    vi.mocked(listen).mockImplementation(((_event: string, handler: () => void) => {
      onNavigate = handler;
      return Promise.resolve(() => {});
    }) as unknown as typeof listen);
    render(<TabLayout />);
    expect(screen.getByText("overview page")).toBeVisible();
    expect(onNavigate).toBeDefined();

    act(() => onNavigate!());
    expect(screen.getByText("settings page")).toBeVisible();
    expect(screen.getByRole("tab", { name: "Settings" })).toHaveAttribute("aria-selected", "true");
  });
});
