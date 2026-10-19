declare module "@earendil-works/pi-coding-agent" {
  export interface ExtensionAPI {
    registerTool?(tool: unknown): void;
    registerCommand?(name: string, options: unknown): void;
    on(event: string, handler: (...args: any[]) => any): void;
  }
  export interface ExtensionContext {
    ui: {
      notify(message: string, level?: "info" | "warning" | "error"): void;
      confirm?(title: string, message: string): Promise<boolean>;
    };
    sessionManager?: {
      getSessionFile?(): string;
      getSessionDir?(): string;
    };
  }
}

declare module "typebox" {
  export const Type: {
    Object(properties: Record<string, unknown>): unknown;
    Union(items: unknown[], options?: Record<string, unknown>): unknown;
    Literal(value: string): unknown;
    String(options?: Record<string, unknown>): unknown;
    Number(options?: Record<string, unknown>): unknown;
    Boolean(options?: Record<string, unknown>): unknown;
    Array(item: unknown, options?: Record<string, unknown>): unknown;
    Optional(schema: unknown): unknown;
  };
}
