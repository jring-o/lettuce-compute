import { render, screen } from "@testing-library/react";

// Mock next/link
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

// Mock next-auth/react (used by MobileMenu)
jest.mock("next-auth/react", () => ({
  signOut: jest.fn(),
}));

// Mock auth module
const mockAuth = jest.fn();
const mockSignOut = jest.fn();
jest.mock("@/lib/auth", () => ({
  auth: (...args: unknown[]) => mockAuth(...args),
  signOut: (...args: unknown[]) => mockSignOut(...args),
}));

// Mock lucide-react icons
jest.mock("lucide-react", () => ({
  Menu: () => <span data-testid="menu-icon">Menu</span>,
  X: () => <span data-testid="x-icon">X</span>,
}));

import { Navbar } from "@/components/layout/navbar";

describe("Navbar", () => {
  beforeEach(() => {
    jest.clearAllMocks();
  });

  it("renders logo with link to home", async () => {
    mockAuth.mockResolvedValue(null);

    const page = await Navbar();
    render(page);

    const logo = screen.getByText("Lettuce");
    expect(logo).toBeInTheDocument();
    expect(logo.closest("a")).toHaveAttribute("href", "/");
  });

  it("renders Projects navigation link", async () => {
    mockAuth.mockResolvedValue(null);

    const page = await Navbar();
    render(page);

    const projectsLinks = screen.getAllByText("Leafs");
    expect(projectsLinks.length).toBeGreaterThan(0);
  });

  it("renders Contribute navigation link", async () => {
    mockAuth.mockResolvedValue(null);

    const page = await Navbar();
    render(page);

    const contributeLinks = screen.getAllByText("Contribute");
    expect(contributeLinks.length).toBeGreaterThan(0);
    const link = contributeLinks[0].closest("a");
    expect(link).toHaveAttribute("href", "/contribute");
  });

  it("shows Sign In when not authenticated", async () => {
    mockAuth.mockResolvedValue(null);

    const page = await Navbar();
    render(page);

    expect(screen.getAllByText("Sign In").length).toBeGreaterThan(0);
  });

  it("shows Dashboard when authenticated", async () => {
    mockAuth.mockResolvedValue({
      user: {
        id: "test-id",
        email: "test@example.com",
        username: "testuser",
        role: "USER",
      },
    });

    const page = await Navbar();
    render(page);

    expect(screen.getAllByText("Dashboard").length).toBeGreaterThan(0);
  });

  it("shows Sign Out button when authenticated", async () => {
    mockAuth.mockResolvedValue({
      user: {
        id: "test-id",
        email: "test@example.com",
        username: "testuser",
        role: "USER",
      },
    });

    const page = await Navbar();
    render(page);

    expect(screen.getAllByText("Sign Out").length).toBeGreaterThan(0);
  });
});
