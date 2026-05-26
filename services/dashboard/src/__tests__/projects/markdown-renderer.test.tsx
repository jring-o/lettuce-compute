import { render, screen } from "@testing-library/react";
import { MarkdownRenderer } from "@/components/projects/markdown-renderer";

describe("MarkdownRenderer", () => {
  it("renders headings", () => {
    render(<MarkdownRenderer content="# Heading 1" />);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent(
      "Heading 1",
    );
  });

  it("renders bold and italic text", () => {
    render(<MarkdownRenderer content="**bold** and *italic*" />);
    expect(screen.getByText("bold")).toBeInTheDocument();
    expect(screen.getByText("italic")).toBeInTheDocument();
  });

  it("renders links with target=_blank and rel=noopener noreferrer", () => {
    render(
      <MarkdownRenderer content="[Example](https://example.com)" />,
    );
    const link = screen.getByRole("link", { name: "Example" });
    expect(link).toHaveAttribute("href", "https://example.com");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link).toHaveAttribute("rel", "noopener noreferrer");
  });

  describe("link href scheme sanitization (XSS prevention)", () => {
    it("strips href from a javascript: link but keeps the text", () => {
      render(
        <MarkdownRenderer content="[click me](javascript:alert(document.cookie))" />,
      );
      const link = screen.getByText("click me");
      expect(link.tagName).toBe("A");
      expect(link).not.toHaveAttribute("href");
      // text remains visible / link is inert
      expect(link.getAttribute("href")).toBeNull();
    });

    it("rejects data: URIs", () => {
      render(
        <MarkdownRenderer content="[x](data:text/html,<script>alert(1)</script>)" />,
      );
      const link = screen.getByText("x");
      expect(link.tagName).toBe("A");
      expect(link).not.toHaveAttribute("href");
    });

    it("rejects vbscript: URIs", () => {
      render(
        <MarkdownRenderer content="[x](vbscript:msgbox(1))" />,
      );
      const link = screen.getByText("x");
      expect(link.tagName).toBe("A");
      expect(link).not.toHaveAttribute("href");
    });

    it("rejects mixed-case obfuscated javascript: schemes", () => {
      render(
        <MarkdownRenderer content="[x](JaVaScRiPt:alert(1))" />,
      );
      const link = screen.getByText("x");
      expect(link).not.toHaveAttribute("href");
    });

    it("rejects whitespace/control-char obfuscated javascript: schemes", () => {
      // Leading spaces plus an embedded tab that browsers strip before parsing.
      render(
        <MarkdownRenderer content={"[x](  java\tscript:alert(1))"} />,
      );
      const link = screen.getByText("x");
      expect(link).not.toHaveAttribute("href");
    });

    it("keeps https:// links", () => {
      render(<MarkdownRenderer content="[x](https://example.com/path)" />);
      expect(screen.getByText("x")).toHaveAttribute(
        "href",
        "https://example.com/path",
      );
    });

    it("keeps http:// links", () => {
      render(<MarkdownRenderer content="[x](http://example.com)" />);
      expect(screen.getByText("x")).toHaveAttribute(
        "href",
        "http://example.com",
      );
    });

    it("keeps mailto: links", () => {
      render(<MarkdownRenderer content="[x](mailto:user@example.com)" />);
      expect(screen.getByText("x")).toHaveAttribute(
        "href",
        "mailto:user@example.com",
      );
    });

    it("keeps relative /path links", () => {
      render(<MarkdownRenderer content="[x](/projects/123)" />);
      expect(screen.getByText("x")).toHaveAttribute("href", "/projects/123");
    });

    it("keeps #anchor links", () => {
      render(<MarkdownRenderer content="[x](#section)" />);
      expect(screen.getByText("x")).toHaveAttribute("href", "#section");
    });
  });

  it("renders code blocks with monospace styling", () => {
    render(
      <MarkdownRenderer
        content={"```javascript\nconsole.log(\"hello\");\n```"}
      />,
    );
    const codeBlock = screen.getByText('console.log("hello");');
    expect(codeBlock).toBeInTheDocument();
    expect(codeBlock.className).toContain("font-mono");
  });

  it("renders tables", () => {
    const table = "| Name | Value |\n|------|-------|\n| foo  | bar   |";
    render(<MarkdownRenderer content={table} />);
    expect(screen.getByText("Name")).toBeInTheDocument();
    expect(screen.getByText("foo")).toBeInTheDocument();
    expect(screen.getByText("bar")).toBeInTheDocument();
  });

  it("escapes HTML (XSS prevention)", () => {
    render(
      <MarkdownRenderer content='<script>alert("xss")</script>' />,
    );
    // Script tags should not be rendered as actual HTML elements
    const container = screen.getByTestId("markdown-content");
    expect(container.innerHTML).not.toContain("<script>");
    expect(container.querySelector("script")).toBeNull();
  });

  it("renders unordered lists", () => {
    render(<MarkdownRenderer content={"- Item 1\n- Item 2"} />);
    expect(screen.getByText("Item 1")).toBeInTheDocument();
    expect(screen.getByText("Item 2")).toBeInTheDocument();
  });

  it("renders ordered lists", () => {
    render(<MarkdownRenderer content={"1. First\n2. Second"} />);
    expect(screen.getByText("First")).toBeInTheDocument();
    expect(screen.getByText("Second")).toBeInTheDocument();
  });

  it("renders blockquotes", () => {
    render(<MarkdownRenderer content="> A quote" />);
    expect(screen.getByText("A quote")).toBeInTheDocument();
  });

  it("renders inline code", () => {
    render(<MarkdownRenderer content="Use `inline code` here" />);
    const code = screen.getByText("inline code");
    expect(code.tagName).toBe("CODE");
  });

  it("renders strikethrough (GFM)", () => {
    render(<MarkdownRenderer content="~~deleted~~" />);
    const del = screen.getByText("deleted");
    expect(del.tagName).toBe("DEL");
  });

  it("applies prose class for typography", () => {
    render(<MarkdownRenderer content="Text" />);
    const container = screen.getByTestId("markdown-content");
    expect(container.className).toContain("prose");
  });

  it("accepts custom className", () => {
    render(<MarkdownRenderer content="Text" className="my-class" />);
    const container = screen.getByTestId("markdown-content");
    expect(container.className).toContain("my-class");
  });
});
