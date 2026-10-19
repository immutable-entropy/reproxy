// The globals a plugin's runtime provides, for an editor's type-checking.
// There is no Node and no DOM: this is all there is besides the language's
// own built-ins (and the SDK, imported from "reproxy").

declare const console: {
  log(...args: unknown[]): void;
  info(...args: unknown[]): void;
  debug(...args: unknown[]): void;
  warn(...args: unknown[]): void;
  error(...args: unknown[]): void;
};

declare function setTimeout<A extends unknown[]>(fn: (...args: A) => void, ms?: number, ...args: A): number;
declare function setInterval<A extends unknown[]>(fn: (...args: A) => void, ms?: number, ...args: A): number;
declare function clearTimeout(id?: number): void;
declare function clearInterval(id?: number): void;
declare function queueMicrotask(fn: () => void): void;
declare function structuredClone<T>(value: T): T;
declare function atob(data: string): string;
declare function btoa(data: string): string;

declare class TextEncoder {
  readonly encoding: "utf-8";
  encode(input?: string): Uint8Array;
}

declare class TextDecoder {
  constructor(label?: "utf-8" | "utf8");
  readonly encoding: "utf-8";
  decode(input?: ArrayBuffer | ArrayBufferView): string;
}
