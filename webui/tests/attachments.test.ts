import test from "node:test";
import assert from "node:assert/strict";
import { checkAttachment, classifyFile, formatBytes, modelAcceptsImages } from "../src/lib/attachments.ts";
import type { Model } from "../src/lib/types.ts";

function file(name: string, type: string, size = 1024): File {
  return new File([new Uint8Array(size)], name, { type });
}

const visionModel: Model = { id: "gpt-vision", provider_id: "p1", input: ["text", "image"] };
const textModel: Model = { id: "deepseek-chat", provider_id: "p2", input: ["text"] };

test("classifyFile maps supported types to their rendering kind", () => {
  assert.equal(classifyFile(file("a.png", "image/png")), "image");
  assert.equal(classifyFile(file("a.jpg", "image/jpeg")), "image");
  assert.equal(classifyFile(file("a.pdf", "application/pdf")), "pdf");
  assert.equal(classifyFile(file("a.md", "text/markdown")), "text");
  assert.equal(classifyFile(file("a.json", "application/json")), "text");
});

test("a content type with parameters still classifies", () => {
  assert.equal(classifyFile(file("a.txt", "text/plain; charset=utf-8")), "text");
});

// Browsers leave type empty for many source files. Rejecting those outright
// would make the console useless for the code people actually want to paste in.
test("a typeless file with an extension is treated as text", () => {
  assert.equal(classifyFile(file("main.go", "")), "text");
  assert.equal(classifyFile(file("Makefile", "")), null);
});

test("unsupported binary types are refused", () => {
  assert.equal(classifyFile(file("a.zip", "application/zip")), null);
  assert.equal(classifyFile(file("a.mp4", "video/mp4")), null);
});

test("modelAcceptsImages reads the catalog's declared modalities", () => {
  assert.equal(modelAcceptsImages(visionModel), true);
  assert.equal(modelAcceptsImages(textModel), false);
  assert.equal(modelAcceptsImages(undefined), false);
});

// The point of checking client-side is that a user learns before spending the
// upload, not after the turn is refused.
test("an image is refused for a model that does not accept images", () => {
  const verdict = checkAttachment(file("shot.png", "image/png"), textModel, 25 * 1024 * 1024);

  assert.equal(verdict.ok, false);
  assert.match(verdict.reason!, /does not accept images/);
});

test("an image is accepted for a vision model", () => {
  assert.equal(checkAttachment(file("shot.png", "image/png"), visionModel, 25 * 1024 * 1024).ok, true);
});

test("text attachments are accepted regardless of model modality", () => {
  assert.equal(checkAttachment(file("notes.md", "text/markdown"), textModel, 25 * 1024 * 1024).ok, true);
});

test("a file over the limit is refused with the limit named", () => {
  const verdict = checkAttachment(file("big.md", "text/markdown", 4096), textModel, 1024);

  assert.equal(verdict.ok, false);
  assert.match(verdict.reason!, /larger than the 1 KB limit/);
});

test("a zero limit disables the size check", () => {
  assert.equal(checkAttachment(file("big.md", "text/markdown", 4096), textModel, 0).ok, true);
});

test("formatBytes picks a readable unit", () => {
  assert.equal(formatBytes(512), "512 B");
  assert.equal(formatBytes(2048), "2 KB");
  assert.equal(formatBytes(5 * 1024 * 1024), "5.0 MB");
});
