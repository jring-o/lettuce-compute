import { render, screen, fireEvent, waitFor, act } from "@testing-library/react";
import { SignInForm } from "@/components/auth/sign-in-form";

const mockPush = jest.fn();
const mockRefresh = jest.fn();
const mockSignIn = jest.fn();

jest.mock("next-auth/react", () => ({
  signIn: (...args: unknown[]) => mockSignIn(...args),
}));

jest.mock("next/navigation", () => ({
  useRouter: () => ({
    push: mockPush,
    refresh: mockRefresh,
  }),
  useSearchParams: () => ({
    get: jest.fn().mockReturnValue(null),
  }),
}));

/** Helper to fill and submit the sign-in form. */
async function fillAndSubmit(
  email: string,
  password: string,
): Promise<void> {
  await act(async () => {
    fireEvent.change(screen.getByLabelText("Email"), {
      target: { name: "email", value: email },
    });
  });
  await act(async () => {
    fireEvent.change(screen.getByLabelText("Password"), {
      target: { name: "password", value: password },
    });
  });
  await act(async () => {
    fireEvent.click(screen.getByRole("button", { name: /Sign In|Signing in/ }));
  });
}

describe("SignInForm submission", () => {
  beforeEach(() => {
    jest.clearAllMocks();
  });

  it("shows validation errors for empty fields on submit", async () => {
    render(<SignInForm />);

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Sign In" }));
    });

    // Email should show an error (email is empty, which is invalid)
    expect(screen.getByText("Invalid email address")).toBeInTheDocument();

    // signIn should not be called when validation fails
    expect(mockSignIn).not.toHaveBeenCalled();
  });

  it("shows validation error for empty password with valid email", async () => {
    render(<SignInForm />);

    await act(async () => {
      fireEvent.change(screen.getByLabelText("Email"), {
        target: { name: "email", value: "user@example.com" },
      });
    });

    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Sign In" }));
    });

    expect(screen.getByText("Password is required")).toBeInTheDocument();
    expect(mockSignIn).not.toHaveBeenCalled();
  });

  it("calls signIn with credentials and redirects on success", async () => {
    mockSignIn.mockResolvedValue({ error: null });

    render(<SignInForm />);
    await fillAndSubmit("user@example.com", "password123");

    await waitFor(() => {
      expect(mockSignIn).toHaveBeenCalledWith("credentials", {
        email: "user@example.com",
        password: "password123",
        redirect: false,
      });
    });

    expect(mockPush).toHaveBeenCalledWith("/dashboard/leafs");
    expect(mockRefresh).toHaveBeenCalled();
  });

  it("shows error message when signIn returns an error", async () => {
    mockSignIn.mockResolvedValue({ error: "CredentialsSignin" });

    render(<SignInForm />);
    await fillAndSubmit("user@example.com", "wrongpassword");

    await waitFor(() => {
      expect(
        screen.getByText("Invalid email or password"),
      ).toBeInTheDocument();
    });

    expect(mockPush).not.toHaveBeenCalled();
  });

  it("shows unexpected error message on network failure", async () => {
    mockSignIn.mockRejectedValue(new Error("Network error"));

    render(<SignInForm />);
    await fillAndSubmit("user@example.com", "password123");

    await waitFor(() => {
      expect(
        screen.getByText("An unexpected error occurred"),
      ).toBeInTheDocument();
    });

    expect(mockPush).not.toHaveBeenCalled();
  });

  it("disables button and shows loading text during submission", async () => {
    let resolveSignIn: (value: unknown) => void;
    mockSignIn.mockReturnValue(
      new Promise((resolve) => {
        resolveSignIn = resolve;
      }),
    );

    render(<SignInForm />);
    await fillAndSubmit("user@example.com", "password123");

    await waitFor(() => {
      const button = screen.getByRole("button", { name: "Signing in..." });
      expect(button).toBeDisabled();
    });

    // Resolve to avoid unhandled promise warning
    await act(async () => {
      resolveSignIn!({ error: null });
    });
  });

  it("clears field error when user types in the errored field", async () => {
    render(<SignInForm />);

    // Submit empty form to trigger validation errors
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Sign In" }));
    });

    expect(screen.getByText("Invalid email address")).toBeInTheDocument();

    // Type in the email field
    await act(async () => {
      fireEvent.change(screen.getByLabelText("Email"), {
        target: { name: "email", value: "u" },
      });
    });

    // The email error should be cleared
    expect(screen.queryByText("Invalid email address")).not.toBeInTheDocument();
  });

  it("clears general error when user types in any field", async () => {
    mockSignIn.mockResolvedValue({ error: "CredentialsSignin" });

    render(<SignInForm />);
    await fillAndSubmit("user@example.com", "wrongpassword");

    await waitFor(() => {
      expect(
        screen.getByText("Invalid email or password"),
      ).toBeInTheDocument();
    });

    // Type in the password field
    await act(async () => {
      fireEvent.change(screen.getByLabelText("Password"), {
        target: { name: "password", value: "x" },
      });
    });

    expect(
      screen.queryByText("Invalid email or password"),
    ).not.toBeInTheDocument();
  });
});
