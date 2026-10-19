/** Keep post-login navigation inside the portal, including hostile query params. */
export function safeLoginReturn(raw: string | null): string {
  if (!raw || !raw.startsWith("/") || raw.startsWith("//") || raw.includes("\\") || /[\u0000-\u0020]/.test(raw)) return "/";
  try {
    const parsed = new URL(raw, "https://console.invalid");
    return parsed.origin === "https://console.invalid" ? `${parsed.pathname}${parsed.search}${parsed.hash}` : "/";
  } catch {
    return "/";
  }
}
