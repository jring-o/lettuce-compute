import { render, screen, fireEvent } from "@testing-library/react";

jest.mock("next/link", () => {
  return function MockLink({
    children,
    href,
  }: {
    children: React.ReactNode;
    href: string;
  }) {
    return <a href={href}>{children}</a>;
  };
});

jest.mock("next-auth/react", () => ({
  signOut: jest.fn(),
}));

jest.mock("lucide-react", () => ({
  Menu: () => <span data-testid="menu-icon">Menu</span>,
  X: () => <span data-testid="x-icon">X</span>,
}));

import { MobileMenu } from "@/components/layout/mobile-menu";

describe("MobileMenu", () => {
  it("renders toggle button", () => {
    render(<MobileMenu user={null} />);

    expect(
      screen.getByRole("button", { name: "Toggle menu" }),
    ).toBeInTheDocument();
  });

  it("shows menu icon when closed", () => {
    render(<MobileMenu user={null} />);

    expect(screen.getByTestId("menu-icon")).toBeInTheDocument();
  });

  it("shows X icon and menu content when opened", () => {
    render(<MobileMenu user={null} />);

    fireEvent.click(screen.getByRole("button", { name: "Toggle menu" }));

    expect(screen.getByTestId("x-icon")).toBeInTheDocument();
  });

  it("shows Sign In link when not authenticated", () => {
    render(<MobileMenu user={null} />);

    fireEvent.click(screen.getByRole("button", { name: "Toggle menu" }));

    expect(screen.getByText("Sign In")).toBeInTheDocument();
  });

  it("shows Projects link in menu", () => {
    render(<MobileMenu user={null} />);

    fireEvent.click(screen.getByRole("button", { name: "Toggle menu" }));

    expect(screen.getByText("Leafs")).toBeInTheDocument();
  });

  it("shows Contribute link in menu", () => {
    render(<MobileMenu user={null} />);

    fireEvent.click(screen.getByRole("button", { name: "Toggle menu" }));

    const link = screen.getByText("Contribute");
    expect(link).toBeInTheDocument();
    expect(link.closest("a")).toHaveAttribute("href", "/contribute");
  });

  it("shows Dashboard and Sign Out when authenticated", () => {
    render(<MobileMenu user={{ username: "janedoe", role: "user" }} />);

    fireEvent.click(screen.getByRole("button", { name: "Toggle menu" }));

    expect(screen.getByText("Dashboard")).toBeInTheDocument();
    expect(screen.getByText("Sign Out")).toBeInTheDocument();
  });

  it("toggles menu closed on second click", () => {
    render(<MobileMenu user={null} />);

    const toggle = screen.getByRole("button", { name: "Toggle menu" });

    // Open
    fireEvent.click(toggle);
    expect(screen.getByText("Leafs")).toBeInTheDocument();

    // Close
    fireEvent.click(toggle);
    expect(screen.queryByText("Leafs")).not.toBeInTheDocument();
  });
});
