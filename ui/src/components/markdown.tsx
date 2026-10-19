import { Fragment, type ReactNode } from "react";
import { cn } from "@/lib/utils";

// Markdown renders a safe, dependency-free subset of Markdown used by project
// memory documents: headings, bullet/numbered lists, code fences, blockquotes,
// horizontal rules, paragraphs, plus inline bold/italic/code/links. Output is
// built from React elements (never dangerouslySetInnerHTML), so content is
// escaped by React and there is no injection surface.

export function Markdown({ content, className }: { content: string; className?: string }) {
  return (
    <div className={cn("space-y-3 text-sm leading-relaxed", className)}>
      {renderBlocks(content)}
    </div>
  );
}

function renderBlocks(src: string): ReactNode[] {
  const lines = src.replace(/\r\n/g, "\n").split("\n");
  // Access helper: the parser only indexes `lines` while `i < lines.length`,
  // so every in-bounds read is defined. `?? ""` satisfies noUncheckedIndexedAccess
  // without papering over a real out-of-bounds (which the loop guards prevent).
  const lineAt = (n: number): string => lines[n] ?? "";
  const out: ReactNode[] = [];
  let i = 0;
  let key = 0;
  const nextKey = () => `b${key++}`;

  while (i < lines.length) {
    const line = lineAt(i);

    // Code fence: ``` ... ```
    if (/^\s*```/.test(line)) {
      const fence: string[] = [];
      i++;
      while (i < lines.length && !/^\s*```/.test(lineAt(i))) {
        fence.push(lineAt(i));
        i++;
      }
      i++; // consume closing fence
      out.push(
        <pre
          key={nextKey()}
          className="overflow-x-auto rounded-md border border-border/60 bg-muted/40 p-3 font-mono text-xs"
        >
          <code>{fence.join("\n")}</code>
        </pre>,
      );
      continue;
    }

    // Blank line
    if (line.trim() === "") {
      i++;
      continue;
    }

    // Horizontal rule
    if (/^\s*(---|\*\*\*|___)\s*$/.test(line)) {
      out.push(<hr key={nextKey()} className="border-border/60" />);
      i++;
      continue;
    }

    // Heading
    const heading = /^(#{1,6})\s+(.*)$/.exec(line);
    if (heading) {
      const level = Math.min((heading[1] ?? "").length, 6);
      const sizes = ["text-xl", "text-lg", "text-base", "text-sm", "text-sm", "text-sm"];
      const cls = cn("font-semibold text-foreground", sizes[level - 1] ?? "text-sm");
      const inner = renderInline(heading[2] ?? "");
      out.push(<Heading key={nextKey()} level={level} className={cls}>{inner}</Heading>);
      i++;
      continue;
    }

    // Blockquote
    if (/^\s*>\s?/.test(line)) {
      const quote: string[] = [];
      while (i < lines.length && /^\s*>\s?/.test(lineAt(i))) {
        quote.push(lineAt(i).replace(/^\s*>\s?/, ""));
        i++;
      }
      out.push(
        <blockquote
          key={nextKey()}
          className="border-l-2 border-border pl-3 text-muted-foreground"
        >
          {quote.map((q, qi) => (
            <p key={qi}>{renderInline(q)}</p>
          ))}
        </blockquote>,
      );
      continue;
    }

    // Unordered list
    if (/^\s*[-*+]\s+/.test(line)) {
      const items: string[] = [];
      while (i < lines.length && /^\s*[-*+]\s+/.test(lineAt(i))) {
        items.push(lineAt(i).replace(/^\s*[-*+]\s+/, ""));
        i++;
      }
      out.push(
        <ul key={nextKey()} className="list-disc space-y-1 pl-5">
          {items.map((it, ii) => (
            <li key={ii}>{renderInline(it)}</li>
          ))}
        </ul>,
      );
      continue;
    }

    // Ordered list
    if (/^\s*\d+\.\s+/.test(line)) {
      const items: string[] = [];
      while (i < lines.length && /^\s*\d+\.\s+/.test(lineAt(i))) {
        items.push(lineAt(i).replace(/^\s*\d+\.\s+/, ""));
        i++;
      }
      out.push(
        <ol key={nextKey()} className="list-decimal space-y-1 pl-5">
          {items.map((it, ii) => (
            <li key={ii}>{renderInline(it)}</li>
          ))}
        </ol>,
      );
      continue;
    }

    // Paragraph: gather consecutive non-blank, non-structural lines
    const para: string[] = [];
    while (
      i < lines.length &&
      lineAt(i).trim() !== "" &&
      !/^\s*```/.test(lineAt(i)) &&
      !/^(#{1,6})\s+/.test(lineAt(i)) &&
      !/^\s*[-*+]\s+/.test(lineAt(i)) &&
      !/^\s*\d+\.\s+/.test(lineAt(i)) &&
      !/^\s*>\s?/.test(lineAt(i)) &&
      !/^\s*(---|\*\*\*|___)\s*$/.test(lineAt(i))
    ) {
      para.push(lineAt(i));
      i++;
    }
    out.push(
      <p key={nextKey()} className="text-foreground/90">
        {renderInline(para.join(" "))}
      </p>,
    );
  }

  return out;
}

function Heading({
  level,
  className,
  children,
}: {
  level: number;
  className?: string;
  children: ReactNode;
}) {
  switch (level) {
    case 1:
      return <h1 className={className}>{children}</h1>;
    case 2:
      return <h2 className={className}>{children}</h2>;
    case 3:
      return <h3 className={className}>{children}</h3>;
    case 4:
      return <h4 className={className}>{children}</h4>;
    case 5:
      return <h5 className={className}>{children}</h5>;
    default:
      return <h6 className={className}>{children}</h6>;
  }
}

// renderInline handles `code`, **bold**, *italic*/_italic_, and [text](url).
// It tokenizes by scanning for the earliest delimiter, recursing into the
// surrounding text. Unmatched delimiters are emitted as literal text.
// SAFE_URL allows only schemes that can't execute script when used as an href.
const SAFE_URL = /^(https?:|mailto:|\/|#|\.\/|\.\.\/)/i;

function renderInline(text: string): ReactNode {
  const nodes: ReactNode[] = [];
  let rest = text;
  let key = 0;
  const push = (n: ReactNode) => nodes.push(<Fragment key={key++}>{n}</Fragment>);

  const patterns: { re: RegExp; render: (m: RegExpExecArray) => ReactNode }[] = [
    { re: /`([^`]+)`/, render: (m) => <code className="rounded bg-muted/60 px-1 py-0.5 font-mono text-[0.85em]">{m[1] ?? ""}</code> },
    { re: /\*\*([^*]+)\*\*/, render: (m) => <strong className="font-semibold">{renderInline(m[1] ?? "")}</strong> },
    { re: /(?:\*([^*]+)\*|_([^_]+)_)/, render: (m) => <em>{renderInline(m[1] ?? m[2] ?? "")}</em> },
    {
      re: /\[([^\]]+)\]\(([^)\s]+)\)/,
      render: (m) => {
        // Allowlist safe URL schemes only — memory content is partly
        // agent-authored, so reject javascript:/data:/vbscript: links that
        // would otherwise become a clickable XSS vector.
        const href = SAFE_URL.test(m[2] ?? "") ? m[2] ?? "" : "#";
        return (
          <a
            href={href}
            target="_blank"
            rel="noopener noreferrer"
            className="text-info underline underline-offset-2"
          >
            {m[1] ?? ""}
          </a>
        );
      },
    },
  ];

  // Iteratively find the earliest-matching pattern and split around it.
  // Bounded by rest shrinking each iteration.
  while (rest.length > 0) {
    let best: { index: number; len: number; node: ReactNode } | null = null;
    for (const p of patterns) {
      const m = p.re.exec(rest);
      if (m && (best === null || m.index < best.index)) {
        best = { index: m.index, len: (m[0] ?? "").length, node: p.render(m) };
      }
    }
    if (!best) {
      push(rest);
      break;
    }
    if (best.index > 0) push(rest.slice(0, best.index));
    push(best.node);
    rest = rest.slice(best.index + best.len);
  }

  return nodes;
}
