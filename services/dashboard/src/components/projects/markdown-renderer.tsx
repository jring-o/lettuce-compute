import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { Components } from "react-markdown";

// Schemes that are safe to navigate to from an author-controlled link.
const ALLOWED_SCHEMES = new Set(["http:", "https:", "mailto:"]);

/**
 * Returns the original href if it is safe to render, otherwise `undefined`
 * (which makes the resulting <a> inert while keeping the link text visible).
 *
 * Allowed: http:, https:, mailto:, and any scheme-relative / relative URL,
 * pure fragment (#anchor) or root/relative path (/path, ./path).
 * Rejected: javascript:, data:, vbscript: and every other explicit scheme,
 * including obfuscated variants using mixed case, leading whitespace, control
 * characters, or embedded tabs/newlines that browsers strip before parsing.
 */
function sanitizeHref(href: string | undefined): string | undefined {
  if (typeof href !== "string") {
    return undefined;
  }

  // Browsers strip ASCII whitespace and C0 control characters (including the
  // ones embedded mid-token, e.g. `java\tscript:`) before resolving a URL.
  // Mirror that so obfuscated schemes cannot slip through. Range U+0000-U+0020
  // covers all C0 control characters plus the space character.
  const normalized = href.replace(/[\u0000-\u0020]/g, "").toLowerCase();

  if (normalized === "") {
    return undefined;
  }

  // If the value declares an explicit URI scheme, it must be on the allowlist.
  const schemeMatch = /^([a-z][a-z0-9+.-]*):/.exec(normalized);
  if (schemeMatch) {
    return ALLOWED_SCHEMES.has(schemeMatch[1] + ":") ? href : undefined;
  }

  // No scheme: relative path, scheme-relative (//host), fragment (#...), or
  // mailto-less reference — all safe to keep.
  return href;
}

const components: Components = {
  a: ({ href, children, ...props }) => {
    const safeHref = sanitizeHref(href);
    return (
      <a
        href={safeHref}
        target="_blank"
        rel="noopener noreferrer"
        {...props}
      >
        {children}
      </a>
    );
  },
  code: ({ className, children, ...props }) => {
    const isBlock = className?.startsWith("language-");
    if (isBlock) {
      return (
        <code
          className={`block rounded bg-muted p-4 font-mono text-sm overflow-x-auto ${className ?? ""}`}
          {...props}
        >
          {children}
        </code>
      );
    }
    return (
      <code
        className="rounded bg-muted px-1.5 py-0.5 font-mono text-sm"
        {...props}
      >
        {children}
      </code>
    );
  },
  pre: ({ children }) => <pre className="not-prose">{children}</pre>,
};

interface MarkdownRendererProps {
  content: string;
  className?: string;
}

export function MarkdownRenderer({ content, className }: MarkdownRendererProps) {
  return (
    <div
      className={`prose prose-sm dark:prose-invert max-w-none ${className ?? ""}`}
      data-testid="markdown-content"
    >
      <ReactMarkdown remarkPlugins={[remarkGfm]} components={components}>
        {content}
      </ReactMarkdown>
    </div>
  );
}
