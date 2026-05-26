import React, { type ReactNode } from "react";

interface ReactMarkdownProps {
  children?: string;
  remarkPlugins?: unknown[];
  components?: Record<string, React.ComponentType<Record<string, unknown>>>;
}

/**
 * Test mock for react-markdown.
 * Renders basic Markdown into React elements for testing.
 * Supports: headings, bold, italic, strikethrough, links, code blocks,
 * inline code, ordered lists, unordered lists, blockquotes, tables.
 */
function ReactMarkdown({ children = "", components = {} }: ReactMarkdownProps) {
  const lines = children.split(/\r?\n/);
  const elements: ReactNode[] = [];
  let i = 0;

  while (i < lines.length) {
    const line = lines[i];

    // Code blocks
    if (line.startsWith("```")) {
      const lang = line.slice(3).trim() || undefined;
      const codeLines: string[] = [];
      i++;
      while (i < lines.length && !lines[i].startsWith("```")) {
        codeLines.push(lines[i]);
        i++;
      }
      i++; // skip closing ```
      const codeContent = codeLines.join("\n");
      const PreComp = components.pre as React.ComponentType<{ children: ReactNode }> | undefined;
      const CodeComp = components.code as React.ComponentType<{ className?: string; children: ReactNode }> | undefined;
      const codeEl = CodeComp
        ? <CodeComp key={`code-${i}`} className={lang ? `language-${lang}` : undefined}>{codeContent}</CodeComp>
        : <code className={lang ? `language-${lang}` : undefined}>{codeContent}</code>;
      const preEl = PreComp
        ? <PreComp key={`pre-${i}`}>{codeEl}</PreComp>
        : <pre key={`pre-${i}`}>{codeEl}</pre>;
      elements.push(preEl);
      continue;
    }

    // Headings
    const headingMatch = line.match(/^(#{1,6})\s+(.+)$/);
    if (headingMatch) {
      const level = headingMatch[1].length;
      const content = renderInline(headingMatch[2], components);
      switch (level) {
        case 1: elements.push(<h1 key={`h-${i}`}>{content}</h1>); break;
        case 2: elements.push(<h2 key={`h-${i}`}>{content}</h2>); break;
        case 3: elements.push(<h3 key={`h-${i}`}>{content}</h3>); break;
        case 4: elements.push(<h4 key={`h-${i}`}>{content}</h4>); break;
        case 5: elements.push(<h5 key={`h-${i}`}>{content}</h5>); break;
        default: elements.push(<h6 key={`h-${i}`}>{content}</h6>); break;
      }
      i++;
      continue;
    }

    // Unordered list
    if (line.match(/^[-*]\s+/)) {
      const items: string[] = [];
      while (i < lines.length && lines[i].match(/^[-*]\s+/)) {
        items.push(lines[i].replace(/^[-*]\s+/, ""));
        i++;
      }
      elements.push(
        <ul key={`ul-${i}`}>
          {items.map((item, j) => (
            <li key={j}>{renderInline(item, components)}</li>
          ))}
        </ul>,
      );
      continue;
    }

    // Ordered list
    if (line.match(/^\d+\.\s+/)) {
      const items: string[] = [];
      while (i < lines.length && lines[i].match(/^\d+\.\s+/)) {
        items.push(lines[i].replace(/^\d+\.\s+/, ""));
        i++;
      }
      elements.push(
        <ol key={`ol-${i}`}>
          {items.map((item, j) => (
            <li key={j}>{renderInline(item, components)}</li>
          ))}
        </ol>,
      );
      continue;
    }

    // Blockquote
    if (line.startsWith("> ")) {
      elements.push(
        <blockquote key={`bq-${i}`}>
          <p>{renderInline(line.slice(2), components)}</p>
        </blockquote>,
      );
      i++;
      continue;
    }

    // Table
    if (line.startsWith("|") && i + 1 < lines.length && lines[i + 1].match(/^\|[\s-:|]+\|$/)) {
      const headerCells = line.split("|").filter(Boolean).map((c) => c.trim());
      i += 2; // skip header + separator
      const bodyRows: string[][] = [];
      while (i < lines.length && lines[i].startsWith("|")) {
        bodyRows.push(lines[i].split("|").filter(Boolean).map((c) => c.trim()));
        i++;
      }
      elements.push(
        <table key={`table-${i}`}>
          <thead>
            <tr>
              {headerCells.map((cell, j) => (
                <th key={j}>{cell}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {bodyRows.map((row, j) => (
              <tr key={j}>
                {row.map((cell, k) => (
                  <td key={k}>{cell}</td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>,
      );
      continue;
    }

    // Empty line
    if (line.trim() === "") {
      i++;
      continue;
    }

    // Paragraph
    elements.push(<p key={`p-${i}`}>{renderInline(line, components)}</p>);
    i++;
  }

  return <>{elements}</>;
}

function renderInline(
  text: string,
  components: Record<string, React.ComponentType<Record<string, unknown>>> = {},
): ReactNode {
  const parts: ReactNode[] = [];
  let remaining = text;
  let key = 0;

  while (remaining.length > 0) {
    // Inline code
    const codeMatch = remaining.match(/^`([^`]+)`/);
    if (codeMatch) {
      parts.push(<code key={key++} className="rounded bg-muted px-1.5 py-0.5 font-mono text-sm">{codeMatch[1]}</code>);
      remaining = remaining.slice(codeMatch[0].length);
      continue;
    }

    // Bold
    const boldMatch = remaining.match(/^\*\*(.+?)\*\*/);
    if (boldMatch) {
      parts.push(<strong key={key++}>{boldMatch[1]}</strong>);
      remaining = remaining.slice(boldMatch[0].length);
      continue;
    }

    // Italic
    const italicMatch = remaining.match(/^\*(.+?)\*/);
    if (italicMatch) {
      parts.push(<em key={key++}>{italicMatch[1]}</em>);
      remaining = remaining.slice(italicMatch[0].length);
      continue;
    }

    // Strikethrough
    const delMatch = remaining.match(/^~~(.+?)~~/);
    if (delMatch) {
      parts.push(<del key={key++}>{delMatch[1]}</del>);
      remaining = remaining.slice(delMatch[0].length);
      continue;
    }

    // Link
    const linkMatch = remaining.match(/^\[([^\]]+)\]\(([^)]+)\)/);
    if (linkMatch) {
      const AComp = components.a as React.ComponentType<{ href: string; children: ReactNode }> | undefined;
      if (AComp) {
        parts.push(<AComp key={key++} href={linkMatch[2]}>{linkMatch[1]}</AComp>);
      } else {
        parts.push(
          <a key={key++} href={linkMatch[2]} target="_blank" rel="noopener noreferrer">
            {linkMatch[1]}
          </a>,
        );
      }
      remaining = remaining.slice(linkMatch[0].length);
      continue;
    }

    // HTML tags — strip them (XSS prevention like react-markdown)
    const htmlMatch = remaining.match(/^<[^>]+>/);
    if (htmlMatch) {
      remaining = remaining.slice(htmlMatch[0].length);
      continue;
    }

    // Plain text — consume until next special char
    const textMatch = remaining.match(/^[^`*~\[<]+/);
    if (textMatch) {
      parts.push(textMatch[0]);
      remaining = remaining.slice(textMatch[0].length);
      continue;
    }

    // Single special char that didn't match any pattern
    parts.push(remaining[0]);
    remaining = remaining.slice(1);
  }

  return parts.length === 1 ? parts[0] : <>{parts}</>;
}

export default ReactMarkdown;
