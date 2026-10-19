import { Fragment, type ReactNode } from "react";
import { parseBlocks, type Block, type ListBlock } from "@/lib/markdown";
import { cn } from "@/lib/utils";

// Markdown renders a safe, dependency-free subset of Markdown: headings,
// nested bullet/numbered lists, tables, code fences, blockquotes, horizontal
// rules, paragraphs, plus inline bold/italic/code/links. The grammar lives in
// lib/markdown.ts; this file only turns blocks into elements. Output is built
// from React elements (never dangerouslySetInnerHTML), so content is escaped by
// React and there is no injection surface.

export function Markdown({ content, className }: { content: string; className?: string }) {
  return (
    <div className={cn("space-y-3 text-sm leading-relaxed", className)}>
      {parseBlocks(content).map((block, i) => (
        <BlockView key={i} block={block} />
      ))}
    </div>
  );
}

function BlockView({ block }: { block: Block }): ReactNode {
  switch (block.type) {
    case "code":
      return (
        <pre className="overflow-x-auto rounded-md border border-border/60 bg-muted/40 p-3 font-mono text-xs">
          <code>{block.text}</code>
        </pre>
      );
    case "rule":
      return <hr className="border-border/60" />;
    case "heading": {
      const sizes = ["text-xl", "text-lg", "text-base", "text-sm", "text-sm", "text-sm"];
      return (
        <Heading
          level={block.level}
          className={cn("font-semibold text-foreground", sizes[block.level - 1] ?? "text-sm")}
        >
          {renderInline(block.text)}
        </Heading>
      );
    }
    case "quote":
      return (
        <blockquote className="border-l-2 border-border pl-3 text-muted-foreground">
          {block.lines.map((line, i) => (
            <p key={i}>{renderInline(line)}</p>
          ))}
        </blockquote>
      );
    case "list":
      return <ListView block={block} />;
    case "table":
      return <TableView block={block} />;
    case "paragraph":
      return <p className="text-foreground/90">{renderInline(block.text)}</p>;
  }
}

function ListView({ block }: { block: ListBlock }) {
  const Tag = block.ordered ? "ol" : "ul";
  return (
    <Tag className={cn("space-y-1 pl-5", block.ordered ? "list-decimal" : "list-disc")}>
      {block.items.map((item, i) => (
        <li key={i}>
          {renderInline(item.text)}
          {item.children.map((child, ci) => (
            // A nested list is its own block inside the item it belongs to,
            // which is what keeps sub-points attached to their parent.
            <div key={ci} className="mt-1">
              <ListView block={child} />
            </div>
          ))}
        </li>
      ))}
    </Tag>
  );
}

/** TableView renders a pipe table. It scrolls in its own box: a wide table must
 *  not widen the message column and give the whole page a horizontal scrollbar. */
function TableView({ block }: { block: Extract<Block, { type: "table" }> }) {
  const alignClass = (i: number) => {
    const align = block.align[i];
    return align === "right" ? "text-right" : align === "center" ? "text-center" : "text-left";
  };
  return (
    <div className="overflow-x-auto rounded-md border border-border/60">
      <table className="w-full border-collapse text-xs">
        <thead className="bg-muted/40">
          <tr>
            {block.head.map((cell, i) => (
              <th
                key={i}
                scope="col"
                className={cn("border-b border-border/60 px-2.5 py-1.5 font-semibold", alignClass(i))}
              >
                {renderInline(cell)}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {block.rows.map((row, ri) => (
            <tr key={ri} className="border-b border-border/40 last:border-0">
              {block.head.map((_, ci) => (
                <td key={ci} className={cn("px-2.5 py-1.5 align-top", alignClass(ci))}>
                  {renderInline(row[ci] ?? "")}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
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
