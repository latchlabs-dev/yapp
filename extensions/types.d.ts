declare module "@earendil-works/pi-coding-agent" {
  export interface ExtensionAPI {
    registerProvider(name: string, config: ProviderConfig): void;
    registerTool?(tool: {
      name: string;
      label: string;
      description: string;
      parameters: unknown;
      execute(
        toolCallId: string,
        params: Record<string, string>,
        signal: unknown,
        onUpdate: unknown,
        ctx: unknown,
      ): Promise<unknown>;
    }): void;
    on(event: string, handler: (...args: any[]) => any): void;
  }

  export interface ExtensionContext {
    ui: {
      notify(message: string, level?: "info" | "warning" | "error"): void;
    };
    mode?: string;
    thinkingLevel?: string;
    sessionManager?: {
      getSessionFile?(): string;
      getSessionDir?(): string;
      getEntries?(): Array<any>;
    };
  }

  export interface ProviderConfig {
    baseUrl: string;
    apiKey: string;
    api: string;
    headers?: Record<string, string>;
    streamSimple?: (...args: any[]) => any;
    models: Array<{
      id: string;
      name: string;
      reasoning: boolean;
      input: Array<"text" | "image">;
      thinkingLevelMap?: Record<string, string | null>;
      cost: {
        input: number;
        output: number;
        cacheRead: number;
        cacheWrite: number;
      };
      contextWindow: number;
      maxTokens: number;
      compat?: Record<string, unknown>;
    }>;
  }
}

declare module "@earendil-works/pi-ai" {
  export function streamSimple(model: any, context: any, options?: any): unknown;
  export function openAICompletionsApi(): {
    streamSimple(model: any, context: any, options?: any): unknown;
  };
  export function openAICodexResponsesApi(): {
    streamSimple(model: any, context: any, options?: any): unknown;
  };
}

declare module "typebox" {
  export const Type: {
    Object(schema: Record<string, unknown>): unknown;
    Union(items: unknown[], options?: Record<string, unknown>): unknown;
    Literal(value: string): unknown;
    String(options?: Record<string, unknown>): unknown;
    Optional(schema: unknown): unknown;
  };
}

declare const process: {
  env: Record<string, string | undefined>;
  cwd(): string;
};

declare const console: {
  log(message?: unknown, ...optionalParams: unknown[]): void;
};

declare function setInterval(handler: (...args: any[]) => void, timeout?: number): unknown;
declare function clearInterval(intervalID: unknown): void;
declare function setTimeout(handler: (...args: any[]) => void, timeout?: number): unknown;
declare function clearTimeout(timeoutID: unknown): void;

declare module "node:assert/strict" {
  const assert: {
    equal(actual: unknown, expected: unknown, message?: string): void;
    deepEqual(actual: unknown, expected: unknown, message?: string): void;
    ok(value: unknown, message?: string): void;
    match(actual: string, expected: RegExp, message?: string): void;
  };
  export default assert;
}

declare module "node:test" {
  export function test(name: string, fn: () => void | Promise<void>): void;
  export function beforeEach(fn: () => void | Promise<void>): void;
  export function afterEach(fn: () => void | Promise<void>): void;
}

declare module "node:fs" {
  export function existsSync(path: string): boolean;
  export function readFileSync(path: string, encoding: string): string;
}

declare module "node:fs/promises" {
  export function mkdtemp(prefix: string): Promise<string>;
  export function mkdir(path: string, options: { recursive?: boolean }): Promise<void>;
  export function rm(path: string, options: { recursive?: boolean; force?: boolean }): Promise<void>;
  export function readFile(path: string): Promise<Uint8Array>;
  export function readFile(path: string, encoding: string): Promise<string>;
  export function readdir(path: string, options: { withFileTypes: true }): Promise<Array<{
    name: string;
    isDirectory(): boolean;
    isFile(): boolean;
  }>>;
  export function stat(path: string): Promise<{
    size: number;
    mtimeMs: number;
    isFile(): boolean;
  }>;
  export function writeFile(path: string, data: string, encoding: string): Promise<void>;
  export function rename(oldPath: string, newPath: string): Promise<void>;
}

declare module "node:crypto" {
  export function createHash(algorithm: string): {
    update(data: Uint8Array): { digest(encoding: "hex"): string };
  };
}

declare module "node:child_process" {
  export function execFile(
    file: string,
    args: string[],
    options: { timeout?: number; maxBuffer?: number },
    callback: (error: Error | null, stdout: string, stderr: string) => void,
  ): void;
}

declare module "node:util" {
  export function promisify(fn: typeof import("node:child_process").execFile): (
    file: string,
    args: string[],
    options: { timeout?: number; maxBuffer?: number },
  ) => Promise<{ stdout: string; stderr: string }>;
}

declare module "node:os" {
  export function homedir(): string;
  export function tmpdir(): string;
}

declare module "node:zlib" {
  export function gzip(data: Uint8Array, callback: (err: Error | null, result: Uint8Array) => void): void;
  export function gunzipSync(data: Uint8Array): Uint8Array;
}

declare module "node:path" {
  export function join(...paths: string[]): string;
  export function dirname(path: string): string;
  export function basename(path: string): string;
}
