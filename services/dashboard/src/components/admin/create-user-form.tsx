"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { createUser } from "@/lib/actions/admin";

export function CreateUserForm() {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState<string | null>(null);
  const [isPending, startTransition] = useTransition();

  const [username, setUsername] = useState("");
  const [email, setEmail] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<"USER" | "ADMIN">("USER");

  function resetForm() {
    setUsername("");
    setEmail("");
    setDisplayName("");
    setPassword("");
    setRole("USER");
    setError(null);
    setSuccess(null);
  }

  function handleSubmit() {
    setError(null);
    setSuccess(null);
    startTransition(async () => {
      const result = await createUser({
        username,
        email,
        displayName: displayName || undefined,
        password,
        role,
      });
      if ("error" in result) {
        setError(result.error.message);
        return;
      }
      resetForm();
      setSuccess(`User "${result.data.username}" created successfully.`);
      router.refresh();
    });
  }

  if (!open) {
    return (
      <Button onClick={() => setOpen(true)} size="sm">
        Create User
      </Button>
    );
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50">
      <div className="mx-4 w-full max-w-md rounded-lg border bg-background p-6">
        <h3 className="text-lg font-semibold">Create User</h3>
        <p className="mt-1 text-sm text-muted-foreground">
          Create a new account. Share the credentials with the user out-of-band.
        </p>

        <div className="mt-4 space-y-4">
          <div>
            <Label htmlFor="create-username">Username</Label>
            <Input
              id="create-username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder="e.g. jsmith"
              className="mt-1"
              maxLength={50}
            />
            <p className="mt-0.5 text-xs text-muted-foreground">
              Lowercase letters, numbers, and hyphens. Must start with a letter.
            </p>
          </div>

          <div>
            <Label htmlFor="create-email">Email</Label>
            <Input
              id="create-email"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="user@example.com"
              className="mt-1"
            />
          </div>

          <div>
            <Label htmlFor="create-display-name">Display Name (optional)</Label>
            <Input
              id="create-display-name"
              value={displayName}
              onChange={(e) => setDisplayName(e.target.value)}
              placeholder="Jane Smith"
              className="mt-1"
              maxLength={100}
            />
          </div>

          <div>
            <Label htmlFor="create-password">Temporary Password</Label>
            <Input
              id="create-password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="Minimum 8 characters"
              className="mt-1"
            />
          </div>

          <div>
            <Label htmlFor="create-role">Role</Label>
            <div className="mt-1 flex gap-2">
              <Button
                type="button"
                variant={role === "USER" ? "default" : "outline"}
                size="sm"
                onClick={() => setRole("USER")}
              >
                User
              </Button>
              <Button
                type="button"
                variant={role === "ADMIN" ? "default" : "outline"}
                size="sm"
                onClick={() => setRole("ADMIN")}
              >
                Admin
              </Button>
            </div>
          </div>
        </div>

        {error && (
          <p className="mt-3 text-sm text-destructive">{error}</p>
        )}
        {success && (
          <p className="mt-3 text-sm text-green-600 dark:text-green-400">{success}</p>
        )}

        <div className="mt-6 flex justify-end gap-2">
          <Button
            variant="ghost"
            onClick={() => {
              setOpen(false);
              resetForm();
            }}
          >
            {success ? "Done" : "Cancel"}
          </Button>
          {!success && (
            <Button
              onClick={handleSubmit}
              disabled={isPending || !username.trim() || !email.trim() || !password.trim()}
            >
              {isPending ? "Creating..." : "Create User"}
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}
