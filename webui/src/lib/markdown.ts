// Block parsing for the console's Markdown subset. Kept free of React so the
// grammar can be unit-tested directly; markdown.tsx turns these blocks into
// escaped React elements and owns inline formatting.

export type Align = "left" | "center" | "right";

export interface ListItem {
  text: string;
  /** Nested lists belonging to this item. */
  children: ListBlock[];
}

export interface ListBlock {
  type: "list";
  ordered: boolean;
  items: ListItem[];
}

export type Block =
  | { type: "code"; text: string; lang: string }
  | { type: "heading"; level: number; text: string }
  | { type: "rule" }
  | { type: "quote"; lines: string[] }
  | { type: "table"; head: string[]; align: (Align | null)[]; rows: string[][] }
  | { type: "paragraph"; text: string }
  | ListBlock;

const FENCE = /^\s*```(.*)$/;
const HEADING = /^(#{1,6})\s+(.*)$/;
const RULE = /^\s*(---|\*\*\*|___)\s*$/;
const QUOTE = /^\s*>\s?/;
const LIST_ITEM = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/;
// A delimiter row is what distinguishes a table from a paragraph containing
// pipes, so it is matched strictly: only dashes, colons, pipes and spaces.
const TABLE_DELIMITER = /^\s*\|?(?:\s*:?-{1,}:?\s*\|)+\s*:?-{1,}:?\s*\|?\s*$/;

export function parseBlocks(src: string): Block[] {
  const lines = src.replace(/\r\n/g, "\n").split("\n");
  const lineAt = (n: number): string => lines[n] ?? "";
  const blocks: Block[] = [];
  let i = 0;

  while (i < lines.length) {
    const line = lineAt(i);

    const fence = FENCE.exec(line);
    if (fence) {
      const body: string[] = [];
      i++;
      while (i < lines.length && !FENCE.test(lineAt(i))) {
        body.push(lineAt(i));
        i++;
      }
      i++; // consume the closing fence
      blocks.push({ type: "code", text: body.join("\n"), lang: (fence[1] ?? "").trim() });
      continue;
    }

    if (line.trim() === "") {
      i++;
      continue;
    }

    if (RULE.test(line)) {
      blocks.push({ type: "rule" });
      i++;
      continue;
    }

    const heading = HEADING.exec(line);
    if (heading) {
      blocks.push({
        type: "heading",
        level: Math.min((heading[1] ?? "").length, 6),
        text: heading[2] ?? "",
      });
      i++;
      continue;
    }

    if (QUOTE.test(line)) {
      const quote: string[] = [];
      while (i < lines.length && QUOTE.test(lineAt(i))) {
        quote.push(lineAt(i).replace(QUOTE, ""));
        i++;
      }
      blocks.push({ type: "quote", lines: quote });
      continue;
    }

    // A table is a header row followed by a delimiter row. Checking the second
    // line first is what keeps ordinary prose containing a pipe out of here.
    if (line.includes("|") && TABLE_DELIMITER.test(lineAt(i + 1))) {
      const head = splitRow(line);
      const align = splitRow(lineAt(i + 1)).map(alignOf);
      i += 2;
      const rows: string[][] = [];
      while (i < lines.length && lineAt(i).includes("|") && lineAt(i).trim() !== "") {
        rows.push(splitRow(lineAt(i)));
        i++;
      }
      blocks.push({ type: "table", head, align, rows });
      continue;
    }

    if (LIST_ITEM.test(line)) {
      const parsed = parseList(lines, i, indentOf(line));
      blocks.push(parsed.block);
      i = parsed.next;
      continue;
    }

    // Paragraph: consecutive non-blank, non-structural lines.
    const para: string[] = [];
    while (i < lines.length && lineAt(i).trim() !== "" && !startsBlock(lineAt(i), lineAt(i + 1))) {
      para.push(lineAt(i));
      i++;
    }
    if (para.length === 0) {
      // The line starts a block the loop above would re-enter; never spin.
      para.push(lineAt(i));
      i++;
    }
    blocks.push({ type: "paragraph", text: para.join(" ") });
  }

  return blocks;
}

function startsBlock(line: string, next: string): boolean {
  return (
    FENCE.test(line) ||
    HEADING.test(line) ||
    RULE.test(line) ||
    QUOTE.test(line) ||
    LIST_ITEM.test(line) ||
    (line.includes("|") && TABLE_DELIMITER.test(next))
  );
}

function indentOf(line: string): number {
  return (LIST_ITEM.exec(line)?.[1] ?? "").length;
}

/**
 * parseList consumes one list at the given indent, recursing for deeper items.
 *
 * Nesting is by indentation, which is how it is written; flattening it — the
 * previous behavior — turns a structured answer into a single undifferentiated
 * run of bullets.
 */
function parseList(lines: string[], start: number, indent: number): { block: ListBlock; next: number } {
  const items: ListItem[] = [];
  let ordered: boolean | null = null;
  let i = start;

  while (i < lines.length) {
    const match = LIST_ITEM.exec(lines[i] ?? "");
    if (!match) break;
    const itemIndent = (match[1] ?? "").length;
    if (itemIndent < indent) break;

    if (itemIndent > indent) {
      const nested = parseList(lines, i, itemIndent);
      const parent = items[items.length - 1];
      if (parent) parent.children.push(nested.block);
      else items.push({ text: "", children: [nested.block] });
      i = nested.next;
      continue;
    }

    const isOrdered = /\d/.test((match[2] ?? "").charAt(0));
    if (ordered === null) ordered = isOrdered;
    else if (ordered !== isOrdered) break; // a different marker starts a new list

    items.push({ text: match[3] ?? "", children: [] });
    i++;
  }

  return { block: { type: "list", ordered: ordered ?? false, items }, next: i };
}

/** splitRow splits a table row on unescaped pipes, dropping the outer ones. */
export function splitRow(line: string): string[] {
  const cells: string[] = [];
  let cell = "";
  for (let i = 0; i < line.length; i++) {
    const ch = line[i];
    if (ch === "\\" && line[i + 1] === "|") {
      cell += "|";
      i++;
      continue;
    }
    if (ch === "|") {
      cells.push(cell);
      cell = "";
      continue;
    }
    cell += ch;
  }
  cells.push(cell);
  // A row written with leading and trailing pipes yields empty edge cells.
  if (cells.length > 1 && (cells[0] ?? "").trim() === "") cells.shift();
  if (cells.length > 1 && (cells[cells.length - 1] ?? "").trim() === "") cells.pop();
  return cells.map((c) => c.trim());
}

function alignOf(spec: string): Align | null {
  const left = spec.startsWith(":");
  const right = spec.endsWith(":");
  if (left && right) return "center";
  if (right) return "right";
  if (left) return "left";
  return null;
}
