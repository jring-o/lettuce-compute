import { render, screen } from "@testing-library/react";
import AuthLayout from "@/app/(auth)/layout";

describe("AuthLayout", () => {
  it("renders the Lettuce branding", () => {
    render(
      <AuthLayout>
        <div>child content</div>
      </AuthLayout>,
    );

    expect(screen.getByText("Lettuce")).toBeInTheDocument();
  });

  it("renders the platform tagline", () => {
    render(
      <AuthLayout>
        <div>child content</div>
      </AuthLayout>,
    );

    expect(
      screen.getByText("Distributed Volunteer Compute Platform"),
    ).toBeInTheDocument();
  });

  it("renders children", () => {
    render(
      <AuthLayout>
        <div>child content</div>
      </AuthLayout>,
    );

    expect(screen.getByText("child content")).toBeInTheDocument();
  });
});
