import { render, screen } from "@testing-library/react";

jest.mock("next-auth/react", () => ({
  signIn: jest.fn(),
}));

jest.mock("next/navigation", () => ({
  useRouter: () => ({
    push: jest.fn(),
    refresh: jest.fn(),
  }),
  useSearchParams: () => ({
    get: jest.fn().mockReturnValue(null),
  }),
}));

import SignInPage from "@/app/(auth)/sign-in/page";

describe("SignInPage", () => {
  it("renders the page heading", () => {
    render(<SignInPage />);

    expect(
      screen.getByText("Sign in to your account"),
    ).toBeInTheDocument();
  });

  it("renders the SignInForm component", () => {
    render(<SignInPage />);

    // The form is rendered inside the page
    expect(screen.getByLabelText("Email")).toBeInTheDocument();
    expect(screen.getByLabelText("Password")).toBeInTheDocument();
  });

  it("exports correct metadata", () => {
    // Metadata is a static export, verify it
    const { metadata } = jest.requireActual("@/app/(auth)/sign-in/page");
    expect(metadata.title).toBe("Sign In — Lettuce");
  });
});
