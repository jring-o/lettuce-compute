"use client";

import { useState, useTransition } from "react";
import { useRouter } from "next/navigation";
import {
  Table,
  TableHeader,
  TableBody,
  TableRow,
  TableHead,
  TableCell,
} from "@/components/ui/table";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  deactivateUser,
  reactivateUser,
  updateUserRole,
  resetUserPassword,
  type UserSummary,
} from "@/lib/actions/admin";
import { formatShortDate } from "@/lib/utils";

function ActionButton({
  onClick,
  variant = "outline",
  children,
}: {
  onClick: () => void;
  variant?: "outline" | "destructive";
  children: React.ReactNode;
}) {
  const [isPending, startTransition] = useTransition();
  const router = useRouter();

  function handleClick() {
    startTransition(async () => {
      await onClick();
      router.refresh();
    });
  }

  return (
    <Button variant={variant} size="xs" onClick={handleClick} disabled={isPending}>
      {children}
    </Button>
  );
}

function ResetPasswordButton({ userId }: { userId: string }) {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [isPending, startTransition] = useTransition();

  if (!open) {
    return (
      <Button variant="outline" size="xs" onClick={() => setOpen(true)}>
        Reset Password
      </Button>
    );
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50">
      <div className="mx-4 w-full max-w-sm rounded-lg border bg-background p-6">
        <h3 className="text-lg font-semibold">Reset Password</h3>
        <div className="mt-4">
          <Label htmlFor="new-password">New Password</Label>
          <Input
            id="new-password"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="Minimum 8 characters"
            className="mt-1"
          />
        </div>
        {error && <p className="mt-2 text-sm text-destructive">{error}</p>}
        <div className="mt-4 flex justify-end gap-2">
          <Button
            variant="ghost"
            onClick={() => {
              setOpen(false);
              setPassword("");
              setError(null);
            }}
          >
            Cancel
          </Button>
          <Button
            disabled={isPending || password.length < 8}
            onClick={() => {
              setError(null);
              startTransition(async () => {
                const result = await resetUserPassword(userId, password);
                if ("error" in result) {
                  setError(result.error.message);
                  return;
                }
                setOpen(false);
                setPassword("");
                router.refresh();
              });
            }}
          >
            {isPending ? "Resetting..." : "Reset"}
          </Button>
        </div>
      </div>
    </div>
  );
}

export function UserTable({
  users,
  currentUserId,
}: {
  users: UserSummary[];
  currentUserId: string;
}) {
  if (users.length === 0) {
    return (
      <p className="py-8 text-center text-sm text-muted-foreground">
        No users found.
      </p>
    );
  }

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Username</TableHead>
          <TableHead>Email</TableHead>
          <TableHead>Display Name</TableHead>
          <TableHead>Role</TableHead>
          <TableHead>Created</TableHead>
          <TableHead>Status</TableHead>
          <TableHead className="text-right">Actions</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {users.map((user) => {
          const isDeactivated = user.deactivatedAt !== null;
          const isSelf = user.id === currentUserId;

          return (
            <TableRow
              key={user.id}
              className={isDeactivated ? "opacity-50" : undefined}
            >
              <TableCell className="font-medium">
                {user.username}
                {isSelf && (
                  <span className="ml-1.5 text-xs text-muted-foreground">(you)</span>
                )}
              </TableCell>
              <TableCell>{user.email}</TableCell>
              <TableCell className="text-muted-foreground">
                {user.displayName || "—"}
              </TableCell>
              <TableCell>
                <Badge variant={user.role === "ADMIN" ? "default" : "secondary"}>
                  {user.role}
                </Badge>
              </TableCell>
              <TableCell className="text-muted-foreground">
                {formatShortDate(user.createdAt)}
              </TableCell>
              <TableCell>
                {isDeactivated ? (
                  <Badge variant="destructive">Deactivated</Badge>
                ) : (
                  <Badge variant="secondary">Active</Badge>
                )}
              </TableCell>
              <TableCell>
                {!isSelf && (
                  <div className="flex justify-end gap-1.5">
                    {isDeactivated ? (
                      <ActionButton onClick={() => reactivateUser(user.id)}>
                        Reactivate
                      </ActionButton>
                    ) : (
                      <ActionButton
                        variant="destructive"
                        onClick={() => deactivateUser(user.id)}
                      >
                        Deactivate
                      </ActionButton>
                    )}
                    <ActionButton
                      onClick={() =>
                        updateUserRole(
                          user.id,
                          user.role === "ADMIN" ? "USER" : "ADMIN",
                        )
                      }
                    >
                      {user.role === "ADMIN" ? "Demote" : "Promote"}
                    </ActionButton>
                    <ResetPasswordButton userId={user.id} />
                  </div>
                )}
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}
