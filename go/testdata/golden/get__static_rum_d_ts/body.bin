/**
 * Type declarations for the SOBS RUM (Real User Monitoring) public API.
 *
 * Include in your project:
 *   /// <reference types="./rum" />
 * or add the path to your tsconfig `typeRoots` / `types`.
 *
 * @example
 *   SOBS.init({ endpoint: 'http://YOUR_SOBS_HOST/v1/rum', appName: 'my-app' });
 *   SOBS.captureEvent('checkout', { itemCount: 3 });
 */

// ---------------------------------------------------------------------------
// Init options
// ---------------------------------------------------------------------------

/** Configuration object passed to {@link SOBSApi.init}. */
export interface SOBSInitOptions {
  /** URL of the SOBS RUM ingest endpoint (e.g. `http://host/v1/rum`). */
  endpoint: string;

  /** Human-readable application name attached to every event. */
  appName: string;

  /** Application version string (optional; used for release tracking). */
  appVersion?: string;

  /** Environment label, e.g. `"production"` or `"staging"`. */
  environment?: string;

  /**
   * User identity object. Properties are merged into every RUM event.
   * All fields are optional; omit to collect anonymous telemetry.
   */
  user?: SOBSUserContext;

  /**
   * Maximum number of breadcrumbs retained in the ring buffer.
   * @default 50
   */
  maxBreadcrumbs?: number;

  /**
   * Maximum number of console log entries retained in the ring buffer.
   * @default 20
   */
  maxConsoleLogs?: number;

  /**
   * Sampling rate in the range [0, 1].
   * `1` = capture all events; `0` = capture nothing.
   * @default 1
   */
  sampleRate?: number;

  /** Set `false` to disable automatic page-view tracking. @default true */
  trackPageViews?: boolean;

  /** Set `false` to disable Web Vitals collection. @default true */
  trackWebVitals?: boolean;

  /** Set `false` to disable JS error / unhandled-rejection capture. @default true */
  trackErrors?: boolean;

  /** Set `false` to disable resource timing collection. @default true */
  trackResources?: boolean;

  /** Set `false` to disable console error capture. @default true */
  trackConsole?: boolean;

  /** Set `false` to disable click / interaction breadcrumb collection. @default true */
  trackBreadcrumbs?: boolean;

  /** Set `false` to disable browser context (UA, timezone, locale). @default true */
  enableBrowserContextCollection?: boolean;

  /** Replay recording options. */
  replay?: SOBSReplayOptions;

  /**
   * Bearer token or opaque string used to authenticate browser events when
   * `SOBS_RUM_CLIENT_AUTH_MODE=token` is configured on the server.
   */
  clientAuthToken?: string;
}

// ---------------------------------------------------------------------------
// User context
// ---------------------------------------------------------------------------

/** User identity attached to RUM events. */
export interface SOBSUserContext {
  /** Unique user identifier (e.g. database primary key or UUID). */
  id?: string | number;
  /** Display name or email shown in the SOBS UI. */
  name?: string;
  /** User email address. */
  email?: string;
  /** Any additional custom attributes. */
  [key: string]: unknown;
}

// ---------------------------------------------------------------------------
// Replay options
// ---------------------------------------------------------------------------

/** Options controlling session-replay behaviour. */
export interface SOBSReplayOptions {
  /** Enable rrweb-based session replay. @default false */
  enabled?: boolean;

  /**
   * Custom upload function. Receives the serialised rrweb events and must
   * return a Promise that resolves to the remote asset URL (string) or `null`.
   */
  upload?: (blob: Blob, filename: string) => Promise<string | null>;

  /** URL of the rrweb bundle to load dynamically (if not already on page). */
  scriptUrl?: string;

  /** Flush interval in milliseconds. @default 10000 */
  flushInterval?: number;

  /** Maximum number of rrweb events to buffer before flushing. @default 512 */
  maxEvents?: number;
}

// ---------------------------------------------------------------------------
// Visual / artifact context
// ---------------------------------------------------------------------------

/** Artifact metadata (screenshot, video, etc.) attached to a RUM event. */
export interface SOBSArtifactContext {
  /** Remote URL of the captured asset. */
  url?: string;
  /** MIME type, e.g. `"image/png"`. */
  type?: string;
  /** Human-readable label shown in the SOBS UI. */
  label?: string;
}

/** Replay segment metadata attached to a RUM event. */
export interface SOBSReplayContext {
  /** Remote URL of the rrweb events JSON file. */
  url?: string;
  /** Timestamp (ms since epoch) of the first event in this segment. */
  startTs?: number;
  /** Timestamp (ms since epoch) of the last event in this segment. */
  endTs?: number;
}

/** Combined visual context that links an event to captured artifacts / replays. */
export interface SOBSVisualContext {
  artifact?: SOBSArtifactContext;
  replay?: SOBSReplayContext;
  /** Auto-clear after this many milliseconds. */
  ttlMs?: number;
  /** Clear the context automatically after the next event is sent. @default false */
  consumeOnce?: boolean;
}

// ---------------------------------------------------------------------------
// Breadcrumb / error capture
// ---------------------------------------------------------------------------

/** Additional data passed to {@link SOBSApi.captureException}. */
export interface SOBSExceptionData {
  /** Custom event type override (defaults to `"error"`). */
  type?: string;
  /** Human-readable error message override. */
  message?: string;
  /** Error class name override (e.g. `"TypeError"`). */
  errorType?: string;
  /** Stack trace string override. */
  stack?: string;
  /** Originating source label (e.g. `"captureException"`). */
  errorSource?: string;
  /** Any additional custom attributes. */
  [key: string]: unknown;
}

// ---------------------------------------------------------------------------
// Public API surface
// ---------------------------------------------------------------------------

/**
 * The `SOBS` global object injected by `rum.js`.
 *
 * All methods are safe to call before `init()` completes – they will be
 * buffered and replayed once the SDK initialises.
 */
export interface SOBSApi {
  /**
   * Initialise the SOBS RUM SDK.
   * Must be called once per page load before any other SDK method.
   *
   * @param options - Configuration options.
   */
  init(options: SOBSInitOptions): void;

  /**
   * Send a custom event to the SOBS ingest endpoint.
   *
   * @param eventType - A short label for the event (e.g. `"checkout"`).
   * @param data      - Arbitrary key/value pairs merged into the event payload.
   */
  captureEvent(eventType: string, data?: Record<string, unknown>): void;

  /**
   * Capture a JavaScript error or exception and send it as a RUM error event.
   *
   * @param error - The caught Error object (or any thrown value).
   * @param data  - Optional overrides / extra attributes.
   */
  captureException(error: unknown, data?: SOBSExceptionData): void;

  /**
   * Add a breadcrumb to the in-memory ring buffer. Breadcrumbs are attached to
   * the next error / event that is sent.
   *
   * @param category - Short category label (e.g. `"navigation"`, `"click"`).
   * @param message  - Human-readable description of the action.
   * @param data     - Optional structured metadata.
   */
  addBreadcrumb(category: string, message: string, data?: Record<string, unknown>): void;

  /**
   * Attach a visual context (artifact or replay) to be included in the next
   * RUM event(s).
   *
   * @param data - Visual context descriptor.
   * @returns `true` if the context was accepted, `false` if validation failed.
   */
  setVisualContext(data: SOBSVisualContext): boolean;

  /**
   * Convenience wrapper for {@link setVisualContext} focused on replay metadata.
   *
   * @param replay  - Replay segment descriptor.
   * @param options - TTL / consumeOnce flags.
   * @returns `true` if the context was accepted.
   */
  setReplayContext(
    replay: SOBSReplayContext,
    options?: Pick<SOBSVisualContext, "ttlMs" | "consumeOnce">
  ): boolean;

  /**
   * Convenience wrapper for {@link setVisualContext} focused on artifact metadata.
   *
   * @param artifact - Artifact descriptor.
   * @param options  - TTL / consumeOnce flags.
   * @returns `true` if the context was accepted.
   */
  setArtifactContext(
    artifact: SOBSArtifactContext,
    options?: Pick<SOBSVisualContext, "ttlMs" | "consumeOnce">
  ): boolean;

  /** Clear any pending visual context. */
  clearVisualContext(): void;

  /**
   * Override the distributed trace context attached to RUM events.
   *
   * @param traceId - 32-hex-character W3C trace ID.
   * @param spanId  - 16-hex-character W3C span ID.
   */
  setTraceContext(traceId: string, spanId: string): void;

  /**
   * Set the trace context from a W3C `traceparent` header value.
   *
   * @param traceparent - Traceparent string (`00-<traceId>-<spanId>-<flags>`).
   * @returns `true` if parsing succeeded.
   */
  setTraceParent(traceparent: string): boolean;

  /**
   * Register a custom replay upload function (overrides `init.replay.upload`).
   *
   * @param uploader - Async function that uploads a Blob and returns its URL.
   */
  setReplayUpload(
    uploader: (blob: Blob, filename: string) => Promise<string | null>
  ): void;

  /**
   * Dynamically enable session replay after `init()`.
   * Has no effect if `init()` has not been called yet.
   *
   * @param options - Optional overrides for the replay configuration.
   */
  enableReplay(options?: SOBSReplayOptions): void;

  /** Stop the active replay recorder and clear its state. */
  disableReplay(): void;

  /**
   * Set the client authentication token used to sign asset uploads.
   * Replaces any token provided in `init({ clientAuthToken })`.
   *
   * @param token - Opaque bearer token string.
   */
  setClientAuthToken(token: string): void;
}

export {};
