import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AddServerDialog } from "./add-server-dialog";

const mockClient = {
  attachHead: vi.fn(),
};

vi.mock("@/hooks/use-api", () => ({
  useClient: () => ({ client: mockClient, error: null }),
}));

describe("AddServerDialog", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockClient.attachHead.mockResolvedValue(undefined);
  });

  it("renders URL input and test button when open", () => {
    render(
      <AddServerDialog
        open={true}
        onOpenChange={vi.fn()}
        onServerAdded={vi.fn()}
      />
    );

    expect(
      screen.getByPlaceholderText("https://compute.example.org")
    ).toBeInTheDocument();
    expect(screen.getByText("Test Connection")).toBeInTheDocument();
  });

  it("does not render when closed", () => {
    render(
      <AddServerDialog
        open={false}
        onOpenChange={vi.fn()}
        onServerAdded={vi.fn()}
      />
    );

    expect(
      screen.queryByPlaceholderText("https://compute.example.org")
    ).not.toBeInTheDocument();
  });

  it("test connection button is disabled when URL is empty", () => {
    render(
      <AddServerDialog
        open={true}
        onOpenChange={vi.fn()}
        onServerAdded={vi.fn()}
      />
    );

    const testBtn = screen.getByText("Test Connection");
    expect(testBtn).toBeDisabled();
  });

  it("closes on Escape key", async () => {
    const user = userEvent.setup();
    const onOpenChange = vi.fn();

    render(
      <AddServerDialog
        open={true}
        onOpenChange={onOpenChange}
        onServerAdded={vi.fn()}
      />
    );

    await user.keyboard("{Escape}");
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("closes on Cancel button click", async () => {
    const user = userEvent.setup();
    const onOpenChange = vi.fn();

    render(
      <AddServerDialog
        open={true}
        onOpenChange={onOpenChange}
        onServerAdded={vi.fn()}
      />
    );

    await user.click(screen.getByText("Cancel"));
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("test connection button is enabled after typing URL", async () => {
    const user = userEvent.setup();

    render(
      <AddServerDialog
        open={true}
        onOpenChange={vi.fn()}
        onServerAdded={vi.fn()}
      />
    );

    const input = screen.getByPlaceholderText("https://compute.example.org");
    await user.type(input, "https://example.com");

    const testBtn = screen.getByText("Test Connection");
    expect(testBtn).not.toBeDisabled();
  });

  it("shows head preview after successful test connection", async () => {
    const user = userEvent.setup();
    const mockFetch = vi.fn();

    // Health check succeeds
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve({ status: "healthy" }),
    });
    // Head info succeeds
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          name: "Test Server",
          description: "A test server",
          leafs: [
            { slug: "leaf-a", name: "Leaf A", research_area: "science", state: "ACTIVE" },
          ],
        }),
    });

    vi.stubGlobal("fetch", mockFetch);

    render(
      <AddServerDialog
        open={true}
        onOpenChange={vi.fn()}
        onServerAdded={vi.fn()}
      />
    );

    const input = screen.getByPlaceholderText("https://compute.example.org");
    await user.type(input, "https://example.com");
    await user.click(screen.getByText("Test Connection"));

    await waitFor(() => {
      expect(screen.getByText("Test Server")).toBeInTheDocument();
    });
    expect(screen.getByText("A test server")).toBeInTheDocument();
    expect(screen.getByText("Leaf A")).toBeInTheDocument();

    vi.unstubAllGlobals();
  });

  it("shows test error on failed connection", async () => {
    const user = userEvent.setup();
    const mockFetch = vi.fn();

    mockFetch.mockRejectedValueOnce(new Error("Network error"));
    vi.stubGlobal("fetch", mockFetch);

    render(
      <AddServerDialog
        open={true}
        onOpenChange={vi.fn()}
        onServerAdded={vi.fn()}
      />
    );

    const input = screen.getByPlaceholderText("https://compute.example.org");
    await user.type(input, "https://bad.example.com");
    await user.click(screen.getByText("Test Connection"));

    await waitFor(() => {
      expect(
        screen.getByText("Connection failed. Check the URL and try again.")
      ).toBeInTheDocument();
    });

    vi.unstubAllGlobals();
  });

  it("attach button calls client.attachHead and closes dialog", async () => {
    const user = userEvent.setup();
    const onOpenChange = vi.fn();
    const onServerAdded = vi.fn();
    const mockFetch = vi.fn();

    // Health check succeeds
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve({ status: "healthy" }),
    });
    // Head info succeeds
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          name: "My Server",
          description: "Desc",
          leafs: [{ slug: "l1", name: "Leaf 1", research_area: "bio", state: "ACTIVE" }],
        }),
    });

    vi.stubGlobal("fetch", mockFetch);

    render(
      <AddServerDialog
        open={true}
        onOpenChange={onOpenChange}
        onServerAdded={onServerAdded}
      />
    );

    const input = screen.getByPlaceholderText("https://compute.example.org");
    await user.type(input, "https://my-server.com");
    await user.click(screen.getByText("Test Connection"));

    // Wait for preview to appear, which enables the Attach button
    await waitFor(() => {
      expect(screen.getByText("My Server")).toBeInTheDocument();
    });

    await user.click(screen.getByText("Attach"));

    await waitFor(() => {
      expect(mockClient.attachHead).toHaveBeenCalledWith({
        server_address: "https://my-server.com",
        name: undefined,
      });
    });

    expect(onOpenChange).toHaveBeenCalledWith(false);
    expect(onServerAdded).toHaveBeenCalled();

    vi.unstubAllGlobals();
  });

  it("shows attach error when attachHead fails", async () => {
    const user = userEvent.setup();
    const mockFetch = vi.fn();

    mockClient.attachHead.mockRejectedValue(
      new (await import("@/api/client")).ApiError("ALREADY_ATTACHED", "Already attached")
    );

    // Health check succeeds
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: () => Promise.resolve({ status: "healthy" }),
    });
    // Head info succeeds
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          name: "Dup Server",
          description: "",
          leafs: [],
        }),
    });

    vi.stubGlobal("fetch", mockFetch);

    render(
      <AddServerDialog
        open={true}
        onOpenChange={vi.fn()}
        onServerAdded={vi.fn()}
      />
    );

    const input = screen.getByPlaceholderText("https://compute.example.org");
    await user.type(input, "https://dup.example.com");
    await user.click(screen.getByText("Test Connection"));

    await waitFor(() => {
      expect(screen.getByText("Dup Server")).toBeInTheDocument();
    });

    await user.click(screen.getByText("Attach"));

    await waitFor(() => {
      expect(screen.getByText("Already attached")).toBeInTheDocument();
    });

    vi.unstubAllGlobals();
  });

  it("closes when clicking backdrop overlay", async () => {
    const user = userEvent.setup();
    const onOpenChange = vi.fn();

    render(
      <AddServerDialog
        open={true}
        onOpenChange={onOpenChange}
        onServerAdded={vi.fn()}
      />
    );

    // The backdrop is the fixed overlay div; click it directly
    const backdrop = document.querySelector(".fixed.inset-0");
    expect(backdrop).toBeTruthy();

    // Click the backdrop (not the dialog content)
    await user.click(backdrop!);
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});
