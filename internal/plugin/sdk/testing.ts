// The test harness for plugins: `reproxy plugin test <dir>` runs every
// *.test.ts in a plugin's directory with it.
//
//   import { describe, it, expect, mock } from "reproxy/testing";
//   import { scan } from "../main";
//
//   it("flags a JWT", async () => {
//     const flow = mock.flow({ url: "https://app.test/", responseBody: "eyJhbGciOi..." });
//     await mock.dispatch("traffic.on_response_received", flow);
//     expect(mock.findings()).toHaveLength(1);
//   });
//
// Tests run in the same runtime plugins run in, against a real (empty,
// temporary) engagement: findings merge, URLs template, flows are
// stored. Outbound requests are not sent: mock.respond scripts what comes
// back, and mock.sent lists what was asked. Everything is reset before each
// test.

import type { Entity, EntityType, FileBatch, Finding, Flow, FlowSource, Hooks, RecoveredFile, Target } from "reproxy";

/** A flow to put in the store for a test. */
export interface FlowFixture {
  url: string;
  method?: string;
  requestHeaders?: Record<string, string>;
  requestBody?: string;
  /** 0: no response yet. Default 200 when a response header or body is given. */
  status?: number;
  responseHeaders?: Record<string, string>;
  responseBody?: string;
  source?: FlowSource;
  targetId?: string;
  origin?: string;
  /**
   * The WebSocket messages exchanged over this flow, in order. A websocket
   * message hook dispatched against the flow is called once per message
   * (see mock.dispatch).
   */
  wsMessages?: { direction?: "sent" | "received"; type?: "text" | "binary"; text?: string }[];
}

/**
 * A recovered file to put in the engagement for a test. It is recorded as
 * another recoverer's (author "mock" unless you say otherwise), because a
 * plugin is never called for what it recovered itself.
 */
export interface FileFixture {
  /** Relative to the engagement's recovered files: "app.test/main/src/api.ts". */
  path: string;
  content: string;
  kind?: string;
  notes?: string;
  /** Group files into one recovery, which mock.dispatch can then deliver. */
  batch?: string;
  batchLabel?: string;
  flowId?: number;
  method?: string;
  url?: string;
  host?: string;
  entity?: string;
  source?: "user" | "agent" | "plugin";
  author?: string;
}

/** A request a plugin sent during a test. */
export interface SentRequest {
  url: string;
  method: string;
  raw: string;
  throughProxy: boolean;
}

export interface ScriptedResponse {
  status?: number;
  headers?: Record<string, string>;
  body?: string;
}

interface TestHost {
  flow(f: FlowFixture): Flow;
  index(flowId: number, type: string): Entity;
  dispatch(hook: string, flowId: number): Promise<number>;
  dispatchEntity(hook: string, entityId: number): Promise<number>;
  file(f: FileFixture): RecoveredFile;
  dispatchFile(hook: string, fileId: number): Promise<number>;
  dispatchFileBatch(hook: string, batch: string): Promise<number>;
  action(id: string, flowId: number): Promise<{ ran: boolean; message: string }>;
  target(kind: string, value: string): Target;
  related(): string[];
  sent(): SentRequest[];
  respond(pattern: string, r: ScriptedResponse): void;
  settings(values: Record<string, unknown>): void;
  reset(): void;
}

declare const __reproxy_test: TestHost;
declare const __reproxy_host: {
  findings(q: object): Finding[];
  entities(q: object): Entity[];
  files(q: object): RecoveredFile[];
  fileBatches(): FileBatch[];
};

/**
 * A flow, a flow to make, an entity already in the Explorer, a recovered
 * file, or a recovery (for file.on_batch_complete).
 */
export type Subject = Flow | FlowFixture | Entity | RecoveredFile | FileBatch;

function isEntity(s: Subject): s is Entity {
  return "key" in s;
}

function isFile(s: Subject): s is RecoveredFile {
  return "path" in s && "sha256" in s;
}

function isBatch(s: Subject): s is FileBatch {
  return "files" in s && "label" in s;
}

/** The type of entity a hook is about ("" for entity.*), or undefined for other hooks. */
function hookEntityType(hook: string): EntityType | "" | undefined {
  const prefix = hook.split(".")[0];
  if (prefix === "entity") return "";
  if (prefix === "domain" || prefix === "host" || prefix === "url") return prefix;
  return undefined;
}

function flowId(s: Flow | FlowFixture): number {
  return "id" in s ? s.id : __reproxy_test.flow(s).id;
}

export const mock = {
  /** Put a flow in the store and return it. */
  flow: (f: FlowFixture): Flow => __reproxy_test.flow(f),
  /**
   * Put a flow in the Entity Explorer as core's indexer does — the URL it
   * was a request to (with its host and domain), and the URLs its response
   * links to — and return the entity of the given type the flow reached:
   * its URL by default, or its host or domain. The rest are in
   * mock.entities().
   */
  entity: (flow: Flow | FlowFixture, type: EntityType = "url"): Entity => __reproxy_test.index(flowId(flow), type),
  /**
   * Run the plugin's targets for a hook against a flow (or, for an entity
   * hook, an entity), as core would: only the targets whose match filter
   * accepts it are called. Resolves with how many were, once they have all
   * settled.
   *
   * An entity hook given a flow puts it in the Explorer first (mock.entity)
   * and dispatches the entity of the hook's type it reached there — the
   * URL for url.* and entity.*, the host for host.*, the domain for
   * domain.* — so a test can start from traffic.
   *
   * A websocket message hook is dispatched once per message the flow
   * carries (mock.flow's wsMessages), so one call drives the whole
   * conversation; websocket.on_open and on_close are dispatched for the
   * connection itself.
   */
  dispatch: (hook: keyof Hooks, subject: Subject): Promise<number> => {
    if (isBatch(subject)) return __reproxy_test.dispatchFileBatch(hook, subject.id);
    if (isFile(subject)) return __reproxy_test.dispatchFile(hook, subject.id);
    if (isEntity(subject)) return __reproxy_test.dispatchEntity(hook, subject.id);
    const type = hookEntityType(hook);
    if (type !== undefined) return __reproxy_test.dispatchEntity(hook, __reproxy_test.index(flowId(subject), type || "url").id);
    return __reproxy_test.dispatch(hook, flowId(subject));
  },
  /**
   * Run one of the plugin's actions on a flow, as the traffic list's
   * context menu would. Resolves with ran false, calling nothing, when the
   * action's match filter turns the flow away (the menu wouldn't offer
   * it), and otherwise with what the action answered. Rejects with what it
   * threw.
   */
  action: (id: string, subject: Flow | FlowFixture): Promise<{ ran: boolean; message: string }> =>
    __reproxy_test.action(id, flowId(subject)),
  /**
   * Put a recovered file in the engagement, as another recoverer's, and
   * return it. Dispatch a file hook against it (or against mock.batch, for
   * file.on_batch_complete) to call the plugin.
   */
  file: (f: FileFixture): RecoveredFile => __reproxy_test.file(f),
  /** Every file recovered. */
  files: (): RecoveredFile[] => __reproxy_host.files({}),
  /**
   * The recovery a batch id names, to dispatch file.on_batch_complete
   * with. Give mock.file the same batch for each of its files.
   */
  batch: (id: string): FileBatch | undefined => __reproxy_host.fileBatches().find((b) => b.id === id),
  /** Every finding recorded. */
  findings: (): Finding[] => __reproxy_host.findings({}),
  /** Every entity in the Explorer. */
  entities: (): Entity[] => __reproxy_host.entities({}),
  /**
   * Add a target, as the user would under Targets. The Explorer sees it:
   * a URL added from a host it covers to one it doesn't makes that host
   * Related. Lifecycle hooks such as target.on_changed are not called;
   * call the plugin's function yourself with targets.list().
   */
  target: (kind: Target["kind"], value: string): Target => __reproxy_test.target(kind, value),
  /** The hosts listed under Targets → Related. */
  related: (): string[] => __reproxy_test.related(),
  /** The requests the plugin sent. */
  sent: (): SentRequest[] => __reproxy_test.sent(),
  /** Answer requests whose URL matches pattern (RE2) with r. Unmatched requests get a 404. */
  respond: (pattern: string, r: ScriptedResponse): void => __reproxy_test.respond(pattern, r),
  /** Override settings for this test. */
  settings: (values: Record<string, unknown>): void => __reproxy_test.settings(values),
};

// ---- Registration ---------------------------------------------------------------

type Fn = () => void | Promise<void>;

interface Test {
  name: string;
  fn: Fn;
  before: Fn[];
  after: Fn[];
}

interface Suite {
  name: string;
  before: Fn[];
  after: Fn[];
}

const tests: Test[] = [];
const suites: Suite[] = [{ name: "", before: [], after: [] }];

export function describe(name: string, fn: () => void): void {
  suites.push({ name, before: [], after: [] });
  try {
    fn();
  } finally {
    suites.pop();
  }
}

export function it(name: string, fn: Fn): void {
  const names = suites.map((s) => s.name).filter((n) => n !== "");
  names.push(name);
  const before: Fn[] = [];
  const after: Fn[] = [];
  for (const s of suites) before.push(...s.before);
  for (let i = suites.length - 1; i >= 0; i--) after.push(...suites[i].after);
  tests.push({ name: names.join(" › "), fn, before, after });
}

export const test = it;

export function beforeEach(fn: Fn): void {
  suites[suites.length - 1].before.push(fn);
}

export function afterEach(fn: Fn): void {
  suites[suites.length - 1].after.push(fn);
}

// ---- Assertions -----------------------------------------------------------------

export class AssertionError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "AssertionError";
  }
}

function show(v: unknown): string {
  if (typeof v === "string") return JSON.stringify(v);
  if (v === undefined) return "undefined";
  if (typeof v === "function") return "[function]";
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

function equal(a: unknown, b: unknown): boolean {
  if (Object.is(a, b)) return true;
  if (typeof a !== "object" || typeof b !== "object" || a === null || b === null) return false;
  if (Array.isArray(a) !== Array.isArray(b)) return false;
  const ka = Object.keys(a as object);
  const kb = Object.keys(b as object);
  if (ka.length !== kb.length) return false;
  return ka.every((k) => equal((a as Record<string, unknown>)[k], (b as Record<string, unknown>)[k]));
}

export interface Matchers {
  toBe(want: unknown): void;
  toEqual(want: unknown): void;
  toBeTruthy(): void;
  toBeFalsy(): void;
  toBeUndefined(): void;
  toBeDefined(): void;
  toBeNull(): void;
  toBeGreaterThan(n: number): void;
  toBeLessThan(n: number): void;
  toContain(item: unknown): void;
  toMatch(re: RegExp | string): void;
  toHaveLength(n: number): void;
  toThrow(msg?: string | RegExp): void;
}

export interface Expectation extends Matchers {
  not: Matchers;
  resolves: { [K in keyof Matchers]: (...a: Parameters<Matchers[K]>) => Promise<void> };
  rejects: { [K in keyof Matchers]: (...a: Parameters<Matchers[K]>) => Promise<void> };
}

function matchers(got: unknown, negate: boolean): Matchers {
  const check = (ok: boolean, what: string) => {
    if (ok === negate) throw new AssertionError(`expected ${show(got)} ${negate ? "not " : ""}${what}`);
  };
  return {
    toBe: (want) => check(Object.is(got, want), `to be ${show(want)}`),
    toEqual: (want) => check(equal(got, want), `to equal ${show(want)}`),
    toBeTruthy: () => check(!!got, "to be truthy"),
    toBeFalsy: () => check(!got, "to be falsy"),
    toBeUndefined: () => check(got === undefined, "to be undefined"),
    toBeDefined: () => check(got !== undefined, "to be defined"),
    toBeNull: () => check(got === null, "to be null"),
    toBeGreaterThan: (n) => check((got as number) > n, `to be greater than ${n}`),
    toBeLessThan: (n) => check((got as number) < n, `to be less than ${n}`),
    toContain: (item) =>
      check(
        typeof got === "string"
          ? got.indexOf(String(item)) >= 0
          : Array.isArray(got) && got.some((x) => equal(x, item)),
        `to contain ${show(item)}`,
      ),
    toMatch: (re) => check(typeof got === "string" && (typeof re === "string" ? got.indexOf(re) >= 0 : re.test(got)), `to match ${String(re)}`),
    toHaveLength: (n) => check((got as { length: number } | null)?.length === n, `to have length ${n}`),
    toThrow: (msg) => {
      let thrown: unknown;
      let threw = false;
      try {
        (got as () => unknown)();
      } catch (e) {
        threw = true;
        thrown = e;
      }
      const text = thrown instanceof Error ? thrown.message : String(thrown);
      const ok = threw && (msg === undefined || (typeof msg === "string" ? text.indexOf(msg) >= 0 : msg.test(text)));
      if (ok === negate) throw new AssertionError(`expected the function ${negate ? "not " : ""}to throw${msg ? " " + String(msg) : ""}${threw ? " (threw " + show(text) + ")" : ""}`);
    },
  };
}

function settled(p: unknown, wantReject: boolean): Expectation["resolves"] {
  const out = {} as Record<string, unknown>;
  for (const k of Object.keys(matchers(undefined, false))) {
    out[k] = async (...args: unknown[]) => {
      let value: unknown;
      try {
        value = await p;
        if (wantReject) throw new AssertionError("expected the promise to reject, but it resolved with " + show(value));
      } catch (e) {
        if (!wantReject || e instanceof AssertionError) throw e;
        value = e instanceof Error ? e.message : e;
      }
      (matchers(value, false) as unknown as Record<string, (...a: unknown[]) => void>)[k](...args);
    };
  }
  return out as Expectation["resolves"];
}

export function expect(got: unknown): Expectation {
  return Object.assign(matchers(got, false), {
    not: matchers(got, true),
    resolves: settled(got, false),
    rejects: settled(got, true),
  }) as Expectation;
}

// ---- Running ------------------------------------------------------------------

export interface Result {
  name: string;
  ok: boolean;
  error?: string;
  ms: number;
}

const testTimeoutMs = 5000;

function within<T>(p: Promise<T>, ms: number): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    const t = setTimeout(() => reject(new Error(`timed out after ${ms}ms`)), ms);
    p.then(
      (v) => {
        clearTimeout(t);
        resolve(v);
      },
      (e) => {
        clearTimeout(t);
        reject(e);
      },
    );
  });
}

async function run(filter: string): Promise<Result[]> {
  const re = filter ? new RegExp(filter) : null;
  const results: Result[] = [];
  for (const t of tests) {
    if (re && !re.test(t.name)) continue;
    const start = Date.now();
    try {
      __reproxy_test.reset();
      await within(
        (async () => {
          for (const b of t.before) await b();
          try {
            await t.fn();
          } finally {
            for (const a of t.after) await a();
          }
        })(),
        testTimeoutMs,
      );
      results.push({ name: t.name, ok: true, ms: Date.now() - start });
    } catch (e) {
      const error = e instanceof Error ? (e instanceof AssertionError ? e.message : String(e.stack || e)) : show(e);
      results.push({ name: t.name, ok: false, error, ms: Date.now() - start });
    }
  }
  return results;
}

(globalThis as unknown as { __reproxy_run: typeof run }).__reproxy_run = run;
