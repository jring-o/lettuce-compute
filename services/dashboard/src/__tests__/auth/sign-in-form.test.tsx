import { render, screen } from "@testing-library/react";
import { SignInForm } from "@/components/auth/sign-in-form";

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

describe("SignInForm", () => {
  it("renders email and password fields", () => {
    render(<SignInForm />);

    expect(screen.getByLabelText("Email")).toBeInTheDocument();
    expect(screen.getByLabelText("Password")).toBeInTheDocument();
  });

  it("renders submit button", () => {
    render(<SignInForm />);

    expect(
      screen.getByRole("button", { name: "Sign In" }),
    ).toBeInTheDocument();
  });
});
