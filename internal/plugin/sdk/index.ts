// The reproxy plugin SDK.
//
//   import { findings, log, type Hooks } from "reproxy";
//
// Everything here runs inside reproxy: there is no Node, no filesystem and no
// fetch. What a plugin can do, it does through these functions. Functions
// that wait on something (the network, the engagement's database) return
// promises; the rest return at once.
//
// A plugin's hook functions are its exports, named in plugin.json. Each is
// called with one event object (see Hooks for which) and may be async.

// ---- Models -----------------------------------------------------------------

/** One header line. */
export interface Header {
  name: string;
  value: string;
}

/**
 * One side of an exchange: a request or a response, as it was on the wire.
 * Nothing is decoded until asked for.
 */
export interface Message {
  /** True when there is no message (a request still waiting for its response). */
  readonly empty: boolean;
  /** Bytes the message had on the wire (the capture may hold fewer: truncated). */
  readonly size: number;
  readonly truncated: boolean;
  /** The start line and headers, as text. */
  head(): string;
  /** The request or status line. */
  startLine(): string;
  headers(): Header[];
  /** A header's value (the first, if repeated), any case. */
  header(name: string): string | undefined;
  /** The media type, lower case, without parameters: "application/json". */
  contentType(): string;
  /** The body as text: dechunked and decompressed. "" for a binary body. */
  text(): string;
  /** The body parsed as JSON, or undefined if it isn't JSON. */
  json<T = unknown>(): T | undefined;
  /** True when the body isn't readable as text even decoded. */
  isBinary(): boolean;
  /** The whole message, as captured. */
  raw(): Uint8Array;
  /** The body decoded: dechunked and decompressed, not reformatted. */
  body(): Uint8Array;
  /** The message as text in the form intercept decisions edit (see edit). */
  editable(): string;
}

export type FlowState = "pending" | "complete" | "failed" | "held_request" | "held_response" | "dropped";
export type FlowSource = "proxy" | "replay" | "agent" | "crawl" | "plugin";

/** One captured exchange. */
export interface Flow {
  id: number;
  method: string;
  url: string;
  scheme: "http" | "https";
  host: string;
  /** The path, without the query. */
  path: string;
  query: string;
  state: FlowState;
  /** 0 until a response arrives. */
  status: number;
  statusText: string;
  /** The target whose scope the flow fell in; "" for none. */
  targetId: string;
  source: FlowSource;
  /** The plugin that sent the flow, for plugin traffic. */
  origin: string;
  /** Unix milliseconds. */
  started: number;
  durationMs: number;
  error: string;
  /** True when it was edited while held. */
  edited: boolean;
  request: Message;
  response: Message;
}

export type Location = "request_header" | "request_body" | "response_header" | "response_body";

/** A finding: something the work turned up. */
export interface Finding {
  id: number;
  value: string;
  types: string[];
  notes: string;
  fields: { key: string; value: string }[];
  source: "user" | "agent" | "plugin";
  author: string;
  viewed: boolean;
  /** The value is kept masked in reProxy's own views (see FindingDraft.hidden). */
  hidden: boolean;
  links: { flowId: number; method: string; url: string; location: string; detail: string; added: number }[];
  created: number;
  updated: number;
}

export interface FindingType {
  id: string;
  name: string;
  color: string;
}

/** What a plugin records. Findings with the same value merge into one. */
export interface FindingDraft {
  /** The thing itself: the credential, the leaked field. */
  value: string;
  /** Type IDs or names; types the plugin declares in plugin.json work too. */
  types?: string[];
  /** Why it was raised. Required. */
  notes: string;
  fields?: { key: string; value: string }[] | Record<string, string>;
  /** The flow it was seen in, and where. */
  flowId?: number;
  location?: Location;
  /** Where exactly: a header name, a JSON path. */
  detail?: string;
  /**
   * The value is a secret in itself — a password, a key, a session token, a
   * card number — so reProxy keeps it masked on screen until the user
   * clicks the eye beside it. Set it for anything that would do harm if it
   * were read over a shoulder or caught in a screenshot; leave it off for
   * data that is sensitive but not secret, such as an email address. It
   * changes nothing but the display: the value is still stored, searched
   * and handed to agents and plugins. Recording a hidden value into a
   * finding that already exists hides that finding too.
   */
  hidden?: boolean;
  /**
   * Recovered files to link the finding to, by id (the ids files.addAll
   * returns in Recovery.recorded). The finding then points at each file —
   * the UI opens it, and list_files/get_file read it — so a finding about,
   * say, an exposed source map leads straight to the source recovered from
   * it. A file that no longer exists is skipped.
   */
  files?: number[];
}

/**
 * A file the work recovered: the original source a map carried, a bundle
 * taken apart, anything else pulled out of the traffic and written under
 * the engagement. Its bytes are on disk, not here — read them with
 * files.read.
 */
export interface RecoveredFile {
  id: number;
  /** Where it is, relative to the engagement's recovered files. */
  path: string;
  /** Its base name, and the directory it sits in. */
  name: string;
  dir: string;
  size: number;
  sha256: string;
  /** What it is, from its name and its bytes: "text/typescript", "image/png". */
  mediaType: string;
  /** Whether it can be read as text, so a scanner needn't guess. */
  text: boolean;
  /** What produced it: "source-map", "source-map-dependency", or a plugin's own label. */
  kind: string;
  source: "user" | "agent" | "plugin";
  /** The plugin or agent session that recovered it. */
  author: string;
  /** The recovery it arrived in, and what that recovery was (a map's URL). */
  batch: string;
  batchLabel: string;
  notes: string;
  /** Where it came from. The flow may have been cleared since. */
  flowId: number;
  method: string;
  url: string;
  host: string;
  /** The Explorer entity it belongs to, if any. */
  entity: string;
  created: number;
  updated: number;
}

/** What one files.addAll wrote. */
export interface Recovery {
  /** The batch every file of this recovery shares. */
  batch: string;
  /** The directory they were written under. */
  dir: string;
  /** How many were written or replaced, and how many were already there unchanged. */
  files: number;
  unchanged: number;
  bytes: number;
  /** What was not written, with why: an unsafe path, or a limit reached. */
  skipped: string[];
  recorded: RecoveredFile[];
}

/** One recovery, summarised: what file.on_batch_complete is called with. */
export interface FileBatch {
  id: string;
  label: string;
  files: number;
  bytes: number;
  kind: string;
  source: "user" | "agent" | "plugin";
  author: string;
  host: string;
  flowId: number;
  created: number;
}

/**
 * A file a plugin recovers. Core writes it under the engagement, cleaning
 * the path and keeping it inside the recovered files whatever the path
 * says, and records it — which is what calls the other plugins' file hooks.
 */
export interface FileDraft {
  /**
   * Where to put it, relative to the engagement's recovered files, laying
   * out whatever tree makes sense: "app.test/main/src/api.ts". It comes
   * from traffic, so it is untrusted: core cleans it (a scheme, "..", a
   * drive letter, an illegal character) and refuses one with nothing safe
   * left.
   */
  path: string;
  /** The bytes, as text or as bytes. */
  content: string | Uint8Array;
  /** What produced it, which other plugins' filters can select on. */
  kind?: string;
  notes?: string;
  /**
   * Group the files of one recovery: pass the same batch (any id of your
   * own) for each, and whatever the recovery was as batchLabel. The batch
   * is what a plugin flooded with per-file events reads instead.
   */
  batch?: string;
  batchLabel?: string;
  /** Where it came from; method, url and host are taken from the flow when it is given. */
  flowId?: number;
  method?: string;
  url?: string;
  host?: string;
  /** The Explorer entity it belongs to. */
  entity?: string;
}

/** What an entity is. */
export type EntityType = "domain" | "host" | "url";

/** What every entity has, whatever its type. */
export interface EntityBase {
  id: number;
  type: EntityType;
  /**
   * Identifies it: the domain (example.com), a host's name or address
   * (//api.example.com, //10.0.0.5), an origin (https://api.example.com),
   * or the templated URL. entities.get also takes a bare name or address.
   */
  key: string;
  /** The key of the entity it sits under: a URL's origin, an origin's host, a host's domain; "" for a root. */
  parent: string;
  /** How it is named: the key, but a host's bare name (api.example.com). */
  label: string;
  /** Where it is: a domain has only host (its name); a host has scheme and host; a URL all three. */
  scheme: string;
  host: string;
  path: string;
  targetId: string;
  /** What first put it in the Explorer: "traffic", "link", "referer", "target" or "plugin:<name>". */
  origin: string;
  /** True once a request reached it (or, for a domain or host, anything under it). */
  visited: boolean;
  hits: number;
  /** What discovered it. */
  links: { source: string; fromUrl: string; fromFlowId: number; detail: string }[];
  /** Its key/value store (see entities.set). */
  kv: Record<string, string>;
  firstSeen: number;
  lastSeen: number;
}

/** A registrable domain: example.com. */
export interface Domain extends EntityBase {
  type: "domain";
  /** The public suffix it is registered under: "com", "co.uk". */
  suffix: string;
  /** True for an ICANN suffix, false for a privately run one (github.io). */
  icann: boolean;
}

/**
 * A host. Either a name host — a DNS name or an IP address on its own
 * (scheme "", key //api.example.com), which is what DNS facts are about —
 * or one of the origins served on it (https://api.example.com:8443), which
 * is what traffic reaches. Origins sit under their name host; a name host
 * under its registrable domain, or nothing for an address.
 */
export interface Host extends EntityBase {
  type: "host";
  /** The port an origin is served on; 0 for a name host. */
  port: number;
  /** True when its name is an IP address. */
  ip: boolean;
  /** The Server headers an origin's responses carried. */
  servers: string[];
  /** The IP addresses a name host is known to resolve to (see entities.addHost); each is a host too. */
  addresses: string[];
}

/** A templated URL on a host, with what the traffic to it showed. */
export interface Url extends EntityBase {
  type: "url";
  /** The templated URL: https://host/users/{id}. */
  url: string;
  operations: { method: string; requests: number; query: string[]; body: string[]; statuses: number[] }[];
  examples: string[];
  kinds: string[];
  flowIds: number[];
}

/** An entity in the engagement's Entity Explorer. */
export type Entity = Domain | Host | Url;

/** What a plugin puts in the Explorer. */
export interface UrlDraft {
  /** An absolute URL; its path is templated like any other (/users/12 → /users/{id}). */
  url: string;
  /** The flow whose response pointed at it. */
  flowId?: number;
  /**
   * Where it was learnt of, when no flow pointed at it: an absolute URL
   * (https://example.com/ for a lookup made for that target). Overrides
   * the flow's URL. A URL on a host no target covers is listed under
   * Targets → Related when from is on a host a target covers.
   */
  from?: string;
  /** Where in it: "json:$.links[0].href", "js:fetch". */
  detail?: string;
}

/** A host a plugin puts in the Explorer without a URL. */
export interface HostDraft {
  /** A DNS name or an IP address: no scheme, port or path. */
  host: string;
  /** IP addresses the name resolves to. Each is put in as a host of its own, found the same way. */
  addresses?: string[];
  /** Where it was learnt of (see UrlDraft.from): a host no target covers is Related when from is on one a target does. */
  from?: string;
  /** How it was found: "dns:A". */
  detail?: string;
}

export interface Target {
  id: string;
  kind: "domain" | "wildcard" | "regex" | "ip" | "cidr";
  value: string;
  name: string;
  color: string;
  enabled: boolean;
}

export interface TargetMatch {
  id: string;
  name: string;
  color: string;
}

/** One WebSocket message exchanged after a flow's upgrade. */
export interface WebSocketMessage {
  /** Its position in the connection, from 1. */
  seq: number;
  /** "sent" (browser → server) or "received" (server → browser). */
  direction: "sent" | "received";
  type: "text" | "binary";
  size: number;
  truncated: boolean;
  /** True when a plugin changed it before it was forwarded. */
  edited: boolean;
  /** True when a plugin dropped it (recorded, never forwarded). */
  dropped: boolean;
  /** The payload as text ("" for a binary message). */
  text(): string;
  /** The payload bytes. */
  data(): Uint8Array;
}

/** What http.send resolves with. */
export interface Response extends Message {
  status: number;
  statusText: string;
  ok: boolean;
  durationMs: number;
  /** The flow it was recorded as, or 0 (see SendOptions.record). */
  flowId: number;
}

export interface Request {
  url: string;
  method?: string;
  headers?: Record<string, string> | [string, string][];
  body?: string | Uint8Array;
}

/** A request given as raw HTTP/1.1, sent to url's host. */
export interface RawRequest {
  url: string;
  raw: string;
}

export interface SendOptions {
  /**
   * Send through reproxy's own proxy, so the exchange is recorded (and can
   * be intercepted) like the browser's. Hooks of this plugin don't see it
   * unless a target sets "self"; sends past depth 3 are refused.
   */
  throughProxy?: boolean;
  /** Record a direct send in the traffic list. */
  record?: boolean;
  /** Default 30000. */
  timeoutMs?: number;
}

// ---- Events -----------------------------------------------------------------

export interface FlowEvent {
  hook: string;
  flow: Flow;
}

export interface PendingEvent extends FlowEvent {
  /** The message held: the request or the response. */
  message: Message;
}

/** The hooks and the event each is called with. */
export interface Hooks {
  "plugin.on_load": { hook: string; reason: "startup" | "enable" | "reload" | "rescan"; settings: Record<string, unknown> };
  "plugin.on_unload": { hook: string; reason: "disable" | "reload" | "rescan" | "shutdown" };
  "plugin.on_status_change": { hook: string; from: string; to: string; reason: "user" | "fault" };
  "plugin.on_settings_change": {
    hook: string;
    changed: Record<string, { old: unknown; new: unknown }>;
    settings: Record<string, unknown>;
  };
  "traffic.on_request_received": FlowEvent;
  "traffic.on_request_forwarded": FlowEvent;
  "traffic.on_response_received": FlowEvent;
  "traffic.on_response_forwarded": FlowEvent;
  "traffic.on_flow_complete": FlowEvent;
  "traffic.on_flow_failed": FlowEvent;
  "traffic.on_flow_dropped": FlowEvent;
  "intercept.on_request_pending": PendingEvent;
  "intercept.on_response_pending": PendingEvent;
  "intercept.on_released": { hook: string; flowId: number; direction: "request" | "response" };
  "replay.on_sent": FlowEvent;
  "finding.on_finding_created": { hook: string; finding: Finding };
  "finding.on_finding_updated": { hook: string; finding: Finding };
  "finding.on_finding_removed": { hook: string; finding: Finding };
  "target.on_changed": { hook: string; targets: Target[] };
  "engagement.on_opened": { hook: string; name: string; path: string };
  "engagement.on_closed": { hook: string; name: string; path: string };
  "entity.on_created": { hook: string; entity: Entity };
  "entity.on_updated": { hook: string; entity: Entity };
  "entity.on_deleted": { hook: string; entity: Entity };
  "domain.on_created": { hook: string; entity: Domain };
  "domain.on_updated": { hook: string; entity: Domain };
  "domain.on_deleted": { hook: string; entity: Domain };
  "host.on_created": { hook: string; entity: Host };
  "host.on_updated": { hook: string; entity: Host };
  "host.on_deleted": { hook: string; entity: Host };
  "url.on_created": { hook: string; entity: Url };
  "url.on_updated": { hook: string; entity: Url };
  "url.on_deleted": { hook: string; entity: Url };
  "file.on_created": { hook: string; file: RecoveredFile };
  "file.on_updated": { hook: string; file: RecoveredFile };
  "file.on_deleted": { hook: string; file: RecoveredFile };
  "file.on_batch_complete": { hook: string; batch: FileBatch };
  "websocket.on_open": { hook: string; flow: Flow };
  "websocket.on_message": { hook: string; flow: Flow; message: WebSocketMessage };
  "websocket.on_message_pending": { hook: string; flow: Flow; message: WebSocketMessage };
  "websocket.on_close": { hook: string; flow: Flow };
}

/** The event a WebSocket decide hook (websocket.on_message_pending) answers. */
export interface WebSocketPendingEvent {
  hook: string;
  flow: Flow;
  message: WebSocketMessage;
}

/** The function type for a hook: `export const scan: Handler<"traffic.on_flow_complete"> = ...` */
export type Handler<H extends keyof Hooks> = (event: Hooks[H]) => void | Promise<void>;

/**
 * What an action's function is called with: the flow the user right-clicked
 * in the traffic list (see "actions" in plugin.json). The flow is read
 * fresh when the action runs, so its response is there if it has arrived.
 */
export interface ActionEvent {
  hook: "action";
  /** The action's id in plugin.json, so one function can serve several. */
  action: string;
  flow: Flow;
}

/**
 * What an action may answer: a short message for the user (shown in the
 * status line), or { message }. Throwing (or rejecting) reports the error
 * instead. Anything the action sends goes through http.send, so through the
 * user's upstream proxy chain; send with { throughProxy: true } for the
 * requests to show in the traffic list.
 */
export type ActionResult = string | { message?: string } | void;

/** The function type for an action: `export const retry: ActionHandler = async ({ flow }) => ...` */
export type ActionHandler = (event: ActionEvent) => ActionResult | Promise<ActionResult>;

/**
 * What a command's function is called with: the flags and arguments the
 * user typed, already parsed against what plugin.json declared. Core does
 * the parsing, so a command sees values of the types it asked for and never
 * sees a flag it didn't declare.
 *
 * A plugin's commands answer to the plugin's own name: everything the
 * wordpress plugin declares is `reproxy wordpress <action>`.
 */
export interface CommandEvent {
  hook: "command";
  /** The namespace the user typed: this plugin's name, or an alias of it. */
  namespace: string;
  /** The action, so one function can serve several. */
  action: string;
  /** Every declared flag, defaults included, by name. */
  flags: Record<string, string | number | boolean | string[]>;
  /** The positional arguments, as typed. */
  args: string[];
  /** The user asked for machine-readable output (--json). */
  json: boolean;
}

/**
 * What a command may answer: the text to print, or an object with that text
 * (output), the value to print under --json (json), and the exit status
 * (code, 0 unless it says otherwise). Throwing (or rejecting) prints the
 * error and exits non-zero.
 */
export type CommandResult = string | { output?: string; json?: unknown; code?: number } | void;

/** The function type for a command: `export const ls: CommandHandler = async ({ flags }) => ...` */
export type CommandHandler = (event: CommandEvent) => CommandResult | Promise<CommandResult>;

/** What an intercept hook answers. */
export type Decision =
  | { action: "pass" }
  | { action: "edit"; text?: string; data?: Uint8Array }
  | { action: "forward"; text?: string; data?: Uint8Array }
  | { action: "drop"; reason?: string };

export type DecideHandler = (event: PendingEvent) => Decision | void | Promise<Decision | void>;

// ---- The host -----------------------------------------------------------------

interface Host {
  plugin: { name: string; version: string };
  log(level: string, msg: string): void;
  setting(key: string): unknown;
  settings(): Record<string, unknown>;
  storageGet(scope: string, key: string): Promise<unknown>;
  storageSet(scope: string, key: string, value: unknown): Promise<void>;
  storageDelete(scope: string, key: string): Promise<void>;
  storageKeys(scope: string): Promise<string[]>;
  flow(id: number): Flow | undefined;
  flows(q: FlowQuery): Flow[];
  searchFlows(text: string, regex: boolean, limit: number): Flow[];
  recordFinding(d: FindingDraft): Promise<{ finding: Finding; created: boolean }>;
  findings(q: FindingQuery): Finding[];
  findingTypes(): FindingType[];
  targets(): Target[];
  matchTarget(url: string): TargetMatch | undefined;
  addUrl(d: UrlDraft): Promise<{ entity: Url; created: boolean }>;
  addHost(d: HostDraft): Promise<{ entity: Host; created: boolean }>;
  entity(keyOrUrl: string): Entity | undefined;
  entities(q: EntityQuery): Entity[];
  addFile(d: FileDraft): Promise<{ file: RecoveredFile; created: boolean }>;
  addFiles(d: FileDraft[]): Promise<Recovery>;
  files(q: FileQuery): RecoveredFile[];
  fileBatches(): FileBatch[];
  readFile(ref: number | string | RecoveredFile, max: number): Promise<{ file: RecoveredFile; text: string; truncated: boolean }>;
  setEntityKV(key: string, name: string, value: string): Promise<Entity>;
  deleteEntityKV(key: string, name: string): Promise<Entity>;
  send(r: Request | RawRequest, o: SendOptions): Promise<Response>;
  held(): HeldMessage[];
  forward(flowId: number, text?: string): void;
  drop(flowId: number): void;
  scan(text: string, patterns: (string | Pattern)[], max: number): ScanMatch[];
  urls(text: string, base: string, contentType: string, limit: number): string[];
}

declare const __reproxy_host: Host;
const host = __reproxy_host;

// ---- The API ------------------------------------------------------------------

/** This plugin's name and version. */
export const plugin = host.plugin;

function line(args: unknown[]): string {
  return args
    .map((a) => (typeof a === "string" ? a : a instanceof Error ? String(a.stack || a) : JSON.stringify(a)))
    .join(" ");
}

/** The plugin's log, shown in the Plugins window. console.* goes there too. */
export const log = {
  debug: (...a: unknown[]) => host.log("debug", line(a)),
  info: (...a: unknown[]) => host.log("info", line(a)),
  warn: (...a: unknown[]) => host.log("warn", line(a)),
  error: (...a: unknown[]) => host.log("error", line(a)),
};

/** The plugin's settings, as the user set them (or their defaults). */
export const settings = {
  get<T = unknown>(key: string): T {
    return host.setting(key) as T;
  },
  all(): Record<string, unknown> {
    return host.settings();
  },
};

function store(scope: string) {
  return {
    get: <T = unknown>(key: string) => host.storageGet(scope, key) as Promise<T | undefined>,
    set: (key: string, value: unknown) => host.storageSet(scope, key, value),
    delete: (key: string) => host.storageDelete(scope, key),
    keys: () => host.storageKeys(scope),
  };
}

/**
 * Storage that persists between sessions: values must be JSON. The plugin's
 * own (up to 1 MiB), or the open engagement's (kept in its capture.db,
 * travelling with it).
 */
export const storage = { ...store("global"), engagement: store("engagement") };

export interface FlowQuery {
  targetId?: string;
  host?: string;
  source?: FlowSource;
  /** Newest first; default 200, at most 1000. */
  limit?: number;
}

/** The traffic list. */
export const flows = {
  get: (id: number) => host.flow(id),
  list: (q: FlowQuery = {}) => host.flows(q),
  /** Substring (or regular expression) search over the whole of each message. */
  search: (text: string, o: { regex?: boolean; limit?: number } = {}) => host.searchFlows(text, !!o.regex, o.limit || 200),
};

export interface FindingQuery {
  types?: string[];
  flowId?: number;
}

/** The engagement's findings. */
export const findings = {
  /** Record a finding. One with the same value merges in instead. */
  record: (d: FindingDraft) => host.recordFinding(d),
  list: (q: FindingQuery = {}) => host.findings(q),
  types: () => host.findingTypes(),
};

/** The targets in scope. */
export const targets = {
  list: () => host.targets(),
  /** The target a URL falls under, if any. */
  match: (url: string) => host.matchTarget(url),
};

export interface EntityQuery {
  type?: EntityType;
  targetId?: string;
  host?: string;
  /** Only the entities directly under this key ("" for the roots). */
  parent?: string;
}

/** The engagement's Entity Explorer: its domains, hosts and URLs. */
export const entities = {
  /**
   * Put a URL in the Explorer as discovered (with its host and domain). A
   * URL already there just gains the link.
   */
  addUrl: (d: UrlDraft) => host.addUrl(d),
  /**
   * Put a host in the Explorer as discovered (with its domain), and the
   * addresses it resolves to. One already there gains the link and any new
   * addresses. Resolves with { entity, created } for the name.
   */
  addHost: (d: HostDraft) => host.addHost(d),
  /** The entity with a key, or the URL a concrete URL (https://host/users/12) falls under. */
  get: (keyOrUrl: string) => host.entity(keyOrUrl),
  list: (q: EntityQuery = {}) => host.entities(q),
  /** The entities directly under one. */
  children: (key: string) => host.entities({ parent: key }),
  /**
   * Set a key in an entity's key/value store. Setting the value it already
   * has changes nothing (and calls no update hook).
   */
  set: (key: string, name: string, value: string) => host.setEntityKV(key, name, String(value)),
  /** Remove a key from an entity's key/value store. */
  unset: (key: string, name: string) => host.deleteEntityKV(key, name),
};

export interface FileQuery {
  /** Only the files one recovery produced. */
  batch?: string;
  kind?: string;
  source?: "user" | "agent" | "plugin";
  author?: string;
  host?: string;
  flowId?: number;
  entity?: string;
  /** Only the files under this directory, relative to the recovered files. */
  dir?: string;
  /** Only the files whose path, notes, kind or source URL contain this. */
  text?: string;
  /** Only the files that can be read as text, leaving out fonts and images. */
  textOnly?: boolean;
  /** Newest first; default 200, at most 1000. */
  limit?: number;
}

/**
 * What the work recovered. A plugin decides what is worth recovering and
 * produces the bytes; core writes the file, records it, and calls the file
 * hooks — so a plugin that scans text for secrets can read recovered
 * client source with the code it already has for response bodies.
 *
 * Recovering a path that already holds the same bytes changes nothing and
 * raises no event, so a recovery can be run again safely.
 */
export const files = {
  /** Recover a file: core writes it under the engagement and records it. */
  add: (d: FileDraft) => host.addFile(d),
  /**
   * Recover a whole set of files at once — a source map unpacked, a bundle
   * taken apart. They share one batch, and the recovery ends with a single
   * file.on_batch_complete, which is what a plugin with thousands of files
   * to scan waits for rather than the per-file events. Prefer it to
   * add-in-a-loop: it writes under one lock and enforces the recovery
   * limits (20,000 files, 512 MiB) as a whole.
   */
  addAll: (d: FileDraft[]) => host.addFiles(d),
  list: (q: FileQuery = {}) => host.files(q),
  /** The recoveries the engagement holds, newest first. */
  batches: () => host.fileBatches(),
  /**
   * Read a recovered file's text, by id, by path or by the file itself. At
   * most maxBytes (8 MiB, which is also the ceiling), cut at a character
   * boundary; truncated says whether there was more.
   */
  read: (ref: number | string | RecoveredFile, maxBytes = 0) => host.readFile(ref, maxBytes),
};

/**
 * Outbound HTTP. Requests go out through the configured upstream proxy
 * chain, and by default not through reproxy's own proxy (see
 * SendOptions.throughProxy).
 */
export const http = {
  send: (r: Request | RawRequest, o: SendOptions = {}) => host.send(r, o),
  get: (url: string, o: SendOptions = {}) => host.send({ url }, o),
};

export interface HeldMessage {
  flowId: number;
  direction: "request" | "response";
  message: Message;
}

/** Messages interception holds for the user. */
export const intercept = {
  held: () => host.held(),
  /** Release a held message, edited (in the form Message.editable gives) or as it is. */
  forward: (flowId: number, text?: string) => host.forward(flowId, text),
  drop: (flowId: number) => host.drop(flowId),
};

export interface Pattern {
  name?: string;
  /**
   * A regular expression in Go's RE2 syntax: (?i) for case-insensitive, (?m)
   * for ^ and $ at line breaks. No backreferences or lookaround, and a
   * repeat count is at most 1000 ({1,1000}); in exchange, matching takes
   * time linear in the input, whatever the pattern.
   */
  pattern: string;
}

export interface ScanMatch {
  /** The pattern's name, or the pattern. */
  pattern: string;
  match: string;
  index: number;
  groups: string[];
}

/**
 * Text helpers that run in Go, far faster than the same work in the
 * plugin's own code: use them for scanning bodies.
 */
export const text = {
  /** Every match of the patterns (RE2 syntax, see Pattern), up to max (default 1000). */
  scan: (input: string, patterns: (string | Pattern)[], max = 1000) => host.scan(input, patterns, max),
  /**
   * The http(s) URLs a body refers to, absolute, resolved against base:
   * HTML attributes, quoted paths in scripts and JSON, and absolute URLs.
   */
  urls: (input: string, base: string, contentType = "", limit = 500) => host.urls(input, base, contentType, limit),
};

/** Answers for intercept hooks. */
export const decision = {
  /** No opinion: the next plugin, then the user, decides. */
  pass: (): Decision => ({ action: "pass" }),
  /** Replace the message and let the next plugin, then the user, decide. For a
   *  binary WebSocket message pass a Uint8Array. */
  edit: (text: string | Uint8Array): Decision =>
    typeof text === "string" ? { action: "edit", text } : { action: "edit", data: text },
  /** Send it on now (edited, if given); the user never sees it. */
  forward: (text?: string | Uint8Array): Decision =>
    text === undefined ? { action: "forward" } : typeof text === "string" ? { action: "forward", text } : { action: "forward", data: text },
  /** Drop it now; the user never sees it. */
  drop: (reason?: string): Decision => ({ action: "drop", reason }),
};

/** Edits to a message in its editable form (Message.editable). */
export const edit = {
  setHeader(msg: string, name: string, value: string): string {
    const [head, body] = split(msg);
    const lines = head.split("\n");
    const i = lines.findIndex((l, n) => n > 0 && l.slice(0, l.indexOf(":")).trim().toLowerCase() === name.toLowerCase());
    if (i >= 0) lines[i] = `${name}: ${value}`;
    else lines.push(`${name}: ${value}`);
    return lines.join("\n") + "\n\n" + body;
  },
  removeHeader(msg: string, name: string): string {
    const [head, body] = split(msg);
    const lines = head
      .split("\n")
      .filter((l, n) => n === 0 || l.slice(0, l.indexOf(":")).trim().toLowerCase() !== name.toLowerCase());
    return lines.join("\n") + "\n\n" + body;
  },
  setBody(msg: string, body: string): string {
    return split(msg)[0] + "\n\n" + body;
  },
};

function split(msg: string): [string, string] {
  const norm = msg.replace(/\r\n/g, "\n");
  const i = norm.indexOf("\n\n");
  return i < 0 ? [norm, ""] : [norm.slice(0, i), norm.slice(i + 2)];
}
