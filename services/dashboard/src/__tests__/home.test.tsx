import { render, screen } from "@testing-library/react";

jest.mock("@/lib/auth", () => ({
  auth: jest.fn().mockResolvedValue(null),
}));

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

import Home from "@/app/page";

describe("Home Page", () => {
  it("renders the Lettuce heading", async () => {
    const page = await Home();
    render(page);
    expect(screen.getByText("Lettuce")).toBeInTheDocument();
  });

  it("renders the platform description", async () => {
    const page = await Home();
    render(page);
    expect(
      screen.getByText("Distributed Volunteer Compute Platform"),
    ).toBeInTheDocument();
  });

  it("shows Browse Leafs and Admin Sign In when not authenticated", async () => {
    const page = await Home();
    render(page);
    expect(screen.getByText("Browse Leafs")).toBeInTheDocument();
    expect(screen.getByText("Admin Sign In")).toBeInTheDocument();
  });

  it("shows Browse Leafs and Dashboard when authenticated", async () => {
    const { auth } = jest.requireMock("@/lib/auth");
    auth.mockResolvedValueOnce({
      user: { id: "1", username: "testuser", role: "USER" },
    });

    const page = await Home();
    render(page);
    expect(screen.getByText("Browse Leafs")).toBeInTheDocument();
    expect(screen.getByText("Dashboard")).toBeInTheDocument();
  });
});
