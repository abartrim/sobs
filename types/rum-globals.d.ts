/**
 * Global type augmentations used by the SOBS RUM script.
 *
 * This file teaches TypeScript about:
 *  - Third-party globals injected by rrweb / html2canvas (optional, user-provided)
 *  - SOBS-internal globals set by the RUM script itself
 *  - Legacy Navigator / PerformanceEntry properties used for broad browser support
 */

// ---------------------------------------------------------------------------
// Window augmentations
// ---------------------------------------------------------------------------
interface Window {
  /** SOBS RUM public API, set by rum.js. */
  SOBS: import("./rum").SOBSApi;

  /** Internal sentinel flag that prevents duplicate auto-init. */
  __SOBS_AUTO_INIT_DONE__?: boolean;

  /**
   * Optional trace-parent string injected server-side for distributed tracing.
   * Follows the W3C traceparent format: `00-<traceId>-<spanId>-<flags>`.
   */
  __SOBS_TRACEPARENT__?: string;

  /** Alternative trace-parent key for compatibility with other tracing SDKs. */
  __TRACEPARENT__?: string;

  /**
   * Optional rrweb session-recorder entry-point. When present, the RUM script
   * will use this function instead of loading rrweb dynamically.
   */
  rrwebRecord?: (options: object) => (() => void) | undefined;

  /** Alternative rrweb namespace shape. */
  rrweb?: { record: (options: object) => (() => void) | undefined };

  /**
   * Optional html2canvas function. When present, the RUM script uses it for
   * screenshot capture instead of loading html2canvas dynamically.
   */
  html2canvas?: (element: HTMLElement, options?: object) => Promise<HTMLCanvasElement>;
}

// ---------------------------------------------------------------------------
// Navigator augmentations (legacy cross-browser properties)
// ---------------------------------------------------------------------------
interface Navigator {
  /** IE / pre-standard language property; modern browsers use `language`. */
  userLanguage?: string;

  /**
   * Deprecated platform string (e.g. "Win32", "MacIntel").
   * Still widely supported but removed from newer spec revisions.
   */
  platform?: string;
}

// ---------------------------------------------------------------------------
// PerformanceEntry subtype properties
// ---------------------------------------------------------------------------

/**
 * PerformanceEventTiming adds `interactionId` and `processingStart`
 * which are not part of the base `PerformanceEntry` interface in older TS libs.
 */
interface PerformanceEventTiming extends PerformanceEntry {
  interactionId?: number;
  processingStart: number;
}

/**
 * LayoutShiftEntry adds `hadRecentInput` and `value`
 * (the layout shift score for a single animation frame).
 */
interface LayoutShiftEntry extends PerformanceEntry {
  hadRecentInput: boolean;
  value: number;
}

// ---------------------------------------------------------------------------
// EventTarget narrowing helper
// ---------------------------------------------------------------------------

/**
 * A narrowed EventTarget that may carry resource-loading source attributes.
 * Used when inspecting error events on `<img>`, `<script>`, etc.
 */
interface ResourceEventTarget extends EventTarget {
  currentSrc?: string;
  src?: string;
  href?: string;
}
