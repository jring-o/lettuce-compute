"use client";

import { useState } from "react";
import Link from "next/link";
import { Copy, Check, Terminal } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

interface ContributeSectionProps {
  serverHost: string;
}

export function ContributeSection({ serverHost }: ContributeSectionProps) {
  const [copied, setCopied] = useState(false);
  const attachCommand = `lettuce-volunteer attach --server ${serverHost}`;

  async function handleCopy() {
    await navigator.clipboard.writeText(attachCommand);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }

  return (
    <Card data-testid="contribute-section">
      <CardHeader>
        <CardTitle className="flex items-center gap-2">
          <Terminal className="size-5" />
          How to Contribute
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <ol className="list-decimal list-inside space-y-3 text-sm">
          <li>Install the Lettuce volunteer CLI</li>
          <li>
            Run:{" "}
            <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-sm">
              lettuce-volunteer init
            </code>
          </li>
          <li>
            <div className="inline">
              Run:{" "}
              <span className="inline-flex items-center gap-2">
                <code
                  className="rounded bg-muted px-1.5 py-0.5 font-mono text-sm"
                  data-testid="attach-command"
                >
                  {attachCommand}
                </code>
                <Button
                  variant="ghost"
                  size="icon-xs"
                  onClick={handleCopy}
                  aria-label="Copy attach command"
                  data-testid="copy-button"
                >
                  {copied ? (
                    <Check className="size-3.5" />
                  ) : (
                    <Copy className="size-3.5" />
                  )}
                </Button>
              </span>
            </div>
          </li>
          <li>
            Run:{" "}
            <code className="rounded bg-muted px-1.5 py-0.5 font-mono text-sm">
              lettuce-volunteer start
            </code>
          </li>
        </ol>
        <p className="text-xs text-muted-foreground">
          Your contributions will be automatically tracked and credited.{" "}
          <Link
            href="/"
            className="underline hover:text-foreground"
          >
            Learn more about volunteering
          </Link>
        </p>
      </CardContent>
    </Card>
  );
}
