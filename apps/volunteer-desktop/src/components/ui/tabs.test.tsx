import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Tabs, TabsList, TabsTrigger, TabsContent } from "./tabs";

function renderTabs(defaultValue = "tab1", onValueChange?: (v: string) => void) {
  return render(
    <Tabs defaultValue={defaultValue} onValueChange={onValueChange}>
      <TabsList>
        <TabsTrigger value="tab1">Tab 1</TabsTrigger>
        <TabsTrigger value="tab2">Tab 2</TabsTrigger>
        <TabsTrigger value="tab3">Tab 3</TabsTrigger>
      </TabsList>
      <TabsContent value="tab1">Content 1</TabsContent>
      <TabsContent value="tab2">Content 2</TabsContent>
      <TabsContent value="tab3">Content 3</TabsContent>
    </Tabs>
  );
}

describe("Tabs", () => {
  it("renders the default tab content", () => {
    renderTabs("tab1");
    expect(screen.getByText("Content 1")).toBeInTheDocument();
    expect(screen.queryByText("Content 2")).not.toBeInTheDocument();
    expect(screen.queryByText("Content 3")).not.toBeInTheDocument();
  });

  it("renders a different default tab", () => {
    renderTabs("tab2");
    expect(screen.queryByText("Content 1")).not.toBeInTheDocument();
    expect(screen.getByText("Content 2")).toBeInTheDocument();
  });

  it("switches tab on trigger click", async () => {
    const user = userEvent.setup();
    renderTabs("tab1");

    expect(screen.getByText("Content 1")).toBeInTheDocument();

    await user.click(screen.getByText("Tab 2"));

    expect(screen.queryByText("Content 1")).not.toBeInTheDocument();
    expect(screen.getByText("Content 2")).toBeInTheDocument();
  });

  it("calls onValueChange when tab is clicked", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderTabs("tab1", onChange);

    await user.click(screen.getByText("Tab 3"));
    expect(onChange).toHaveBeenCalledWith("tab3");
  });

  it("marks active trigger with aria-selected=true", () => {
    renderTabs("tab2");
    expect(screen.getByText("Tab 2")).toHaveAttribute("aria-selected", "true");
    expect(screen.getByText("Tab 1")).toHaveAttribute("aria-selected", "false");
  });

  it("renders tablist role on TabsList", () => {
    renderTabs("tab1");
    expect(screen.getByRole("tablist")).toBeInTheDocument();
  });

  it("renders tab role on triggers", () => {
    renderTabs("tab1");
    const tabs = screen.getAllByRole("tab");
    expect(tabs).toHaveLength(3);
  });

  it("renders tabpanel role on active content", () => {
    renderTabs("tab1");
    expect(screen.getByRole("tabpanel")).toBeInTheDocument();
  });

  it("controlled mode uses value prop over internal state", () => {
    render(
      <Tabs defaultValue="tab1" value="tab2">
        <TabsList>
          <TabsTrigger value="tab1">Tab 1</TabsTrigger>
          <TabsTrigger value="tab2">Tab 2</TabsTrigger>
        </TabsList>
        <TabsContent value="tab1">Content 1</TabsContent>
        <TabsContent value="tab2">Content 2</TabsContent>
      </Tabs>
    );
    // value="tab2" overrides defaultValue="tab1"
    expect(screen.queryByText("Content 1")).not.toBeInTheDocument();
    expect(screen.getByText("Content 2")).toBeInTheDocument();
  });
});

describe("TabsTrigger outside Tabs context", () => {
  it("throws error when used outside Tabs", () => {
    // Suppress console.error for expected error
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    expect(() => {
      render(<TabsTrigger value="x">Orphan</TabsTrigger>);
    }).toThrow("useTabs must be used within Tabs");
    spy.mockRestore();
  });
});

describe("TabsContent outside Tabs context", () => {
  it("throws error when used outside Tabs", () => {
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    expect(() => {
      render(<TabsContent value="x">Orphan</TabsContent>);
    }).toThrow("useTabs must be used within Tabs");
    spy.mockRestore();
  });
});
