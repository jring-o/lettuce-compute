"use client";

import { useTransition } from "react";
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
import { revokeApiKey, type ApiKeyInfo } from "@/lib/actions/api-keys";
import { formatShortDate } from "@/lib/utils";

function RevokeButton({ keyId }: { keyId: string }) {
  const router = useRouter();
  const [isPending, startTransition] = useTransition();

  function handleRevoke() {
    startTransition(async () => {
      await revokeApiKey(keyId);
      router.refresh();
    });
  }

  return (
    <Button
      variant="destructive"
      size="xs"
      onClick={handleRevoke}
      disabled={isPending}
    >
      {isPending ? "Revoking..." : "Revoke"}
    </Button>
  );
}

export function ApiKeyTable({ keys }: { keys: ApiKeyInfo[] }) {
  if (keys.length === 0) {
    return (
      <p className="py-8 text-center text-sm text-muted-foreground">
        No API keys yet. Create one to authenticate with the infrastructure API.
      </p>
    );
  }

  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Name</TableHead>
          <TableHead>Key</TableHead>
          <TableHead>Created</TableHead>
          <TableHead>Last Used</TableHead>
          <TableHead>Status</TableHead>
          <TableHead className="w-[1%]" />
        </TableRow>
      </TableHeader>
      <TableBody>
        {keys.map((key) => {
          const revoked = key.revokedAt !== null;
          return (
            <TableRow
              key={key.id}
              className={revoked ? "opacity-50" : undefined}
            >
              <TableCell className="font-medium">{key.name}</TableCell>
              <TableCell>
                <code className="text-xs">{key.keyPrefix}...</code>
              </TableCell>
              <TableCell className="text-muted-foreground">
                {formatShortDate(key.createdAt)}
              </TableCell>
              <TableCell className="text-muted-foreground">
                {formatShortDate(key.lastUsedAt)}
              </TableCell>
              <TableCell>
                {revoked ? (
                  <Badge variant="destructive">Revoked</Badge>
                ) : (
                  <Badge variant="secondary">Active</Badge>
                )}
              </TableCell>
              <TableCell>
                {!revoked && <RevokeButton keyId={key.id} />}
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}
