import { Suspense } from "react";
import { SignInForm } from "@/components/auth/sign-in-form";

export const metadata = {
  title: "Sign In — Lettuce",
};

export default function SignInPage() {
  return (
    <div className="rounded-lg border bg-card p-6 shadow-sm">
      <h2 className="mb-6 text-center text-lg font-semibold">
        Sign in to your account
      </h2>
      <Suspense>
        <SignInForm />
      </Suspense>
    </div>
  );
}
