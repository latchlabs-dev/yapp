import type { Model } from "./types";

/** Types the console accepts. The server re-checks by sniffing the bytes. */
const IMAGE_TYPES = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"]);
const TEXT_PREFIX = "text/";
const TEXT_TYPES = new Set(["application/json", "application/xml", "application/x-yaml", "application/toml"]);

export type AttachmentKind = "image" | "text" | "pdf";

/** classifyFile maps a browser File to the kind the server will assign, or null. */
export function classifyFile(file: File): AttachmentKind | null {
  const type = (file.type || "").split(";")[0]!.trim().toLowerCase();
  if (IMAGE_TYPES.has(type)) return "image";
  if (type === "application/pdf") return "pdf";
  if (type.startsWith(TEXT_PREFIX) || TEXT_TYPES.has(type)) return "text";
  // Browsers leave type empty for many source files; fall back to the
  // extension so a .ts or .go is not rejected out of hand. The server still
  // decides from the bytes.
  if (type === "" && /\.[a-z0-9]{1,8}$/i.test(file.name)) return "text";
  return null;
}

export function modelAcceptsImages(model: Model | undefined): boolean {
  return !!model?.input?.some((m) => m.toLowerCase() === "image");
}

export interface AttachmentCheck {
  ok: boolean;
  /** Why the file cannot be sent, phrased for the user. */
  reason?: string;
}

/**
 * checkAttachment decides whether a file can be attached to the current turn.
 *
 * The image check mirrors the server's, and exists so a user finds out before
 * uploading rather than after: the alternative is a full upload followed by a
 * refusal at send time.
 */
export function checkAttachment(file: File, model: Model | undefined, maxBytes: number): AttachmentCheck {
  const kind = classifyFile(file);
  if (!kind) {
    return { ok: false, reason: `${file.name} is not a supported type — images, text and PDF are accepted` };
  }
  if (maxBytes > 0 && file.size > maxBytes) {
    return { ok: false, reason: `${file.name} is larger than the ${formatBytes(maxBytes)} limit` };
  }
  if (kind === "image" && !modelAcceptsImages(model)) {
    const name = model?.name || model?.id || "the selected model";
    return { ok: false, reason: `${name} does not accept images — choose a vision model to attach one` };
  }
  return { ok: true };
}

export function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
