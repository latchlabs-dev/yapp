import test from "node:test";
import assert from "node:assert/strict";
import { parseBlocks, splitRow, type Block, type ListBlock } from "../src/lib/markdown.ts";

function only<T extends Block["type"]>(src: string, type: T): Extract<Block, { type: T }> {
  const blocks = parseBlocks(src);
  assert.equal(blocks.length, 1, `expected one block, got ${blocks.map((b) => b.type).join(",")}`);
  assert.equal(blocks[0]!.type, type);
  return blocks[0] as Extract<Block, { type: T }>;
}

// A comparison table used to render as a run of pipe-delimited text, which is
// the least readable form of the thing the model was asked to tabulate.
test("a pipe table parses into head, alignment and rows", () => {
  const table = only(
    [
      "| Model | Context | Cost |",
      "| :--- | ---: | :---: |",
      "| corp-a | 128k | $1 |",
      "| corp-b | 200k | $2 |",
    ].join("\n"),
    "table",
  );

  assert.deepEqual(table.head, ["Model", "Context", "Cost"]);
  assert.deepEqual(table.align, ["left", "right", "center"]);
  assert.deepEqual(table.rows, [
    ["corp-a", "128k", "$1"],
    ["corp-b", "200k", "$2"],
  ]);
});

test("a table written without outer pipes still parses", () => {
  const table = only(["a | b", "--- | ---", "1 | 2"].join("\n"), "table");

  assert.deepEqual(table.head, ["a", "b"]);
  assert.deepEqual(table.rows, [["1", "2"]]);
  assert.deepEqual(table.align, [null, null]);
});

// Prose containing a pipe is not a table. Only the delimiter row makes one.
test("a paragraph containing a pipe is not a table", () => {
  const block = only("run `a | b` to pipe the output", "paragraph");
  assert.match(block.text, /a \| b/);
});

test("escaped pipes stay inside their cell", () => {
  assert.deepEqual(splitRow("| a \\| b | c |"), ["a | b", "c"]);
});

// Nested structure used to be flattened into one level of bullets, which loses
// the relationship the author was expressing.
test("indented list items nest under their parent", () => {
  const list = only(
    ["- top", "  - child", "    - grandchild", "- second"].join("\n"),
    "list",
  ) as ListBlock;

  assert.equal(list.ordered, false);
  assert.deepEqual(
    list.items.map((i) => i.text),
    ["top", "second"],
  );
  const child = list.items[0]!.children[0]!;
  assert.equal(child.items[0]!.text, "child");
  assert.equal(child.items[0]!.children[0]!.items[0]!.text, "grandchild");
  assert.equal(list.items[1]!.children.length, 0);
});

test("an ordered list nests an unordered one", () => {
  const list = only(["1. first", "   - detail", "2. second"].join("\n"), "list") as ListBlock;

  assert.equal(list.ordered, true);
  assert.equal(list.items.length, 2);
  assert.equal(list.items[0]!.children[0]!.ordered, false);
  assert.equal(list.items[0]!.children[0]!.items[0]!.text, "detail");
});

test("a list stops at a different marker type on the same level", () => {
  const blocks = parseBlocks(["- bullet", "1. number"].join("\n"));

  assert.deepEqual(
    blocks.map((b) => b.type),
    ["list", "list"],
  );
  assert.equal((blocks[0] as ListBlock).ordered, false);
  assert.equal((blocks[1] as ListBlock).ordered, true);
});

test("code fences keep their language and content verbatim", () => {
  const code = only(["```go", 'fmt.Println("- not a list")', "```"].join("\n"), "code");

  assert.equal(code.lang, "go");
  assert.equal(code.text, 'fmt.Println("- not a list")');
});

test("headings, rules and quotes still parse", () => {
  const blocks = parseBlocks(["## Title", "---", "> quoted", "> more", "plain"].join("\n"));

  assert.deepEqual(
    blocks.map((b) => b.type),
    ["heading", "rule", "quote", "paragraph"],
  );
  assert.equal((blocks[0] as Extract<Block, { type: "heading" }>).level, 2);
  assert.deepEqual((blocks[2] as Extract<Block, { type: "quote" }>).lines, ["quoted", "more"]);
});

test("empty input produces no blocks", () => {
  assert.deepEqual(parseBlocks(""), []);
  assert.deepEqual(parseBlocks("\n\n  \n"), []);
});
