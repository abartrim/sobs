/**
 * jbs-runtime.js  –  Minimal JBS (jinja-bootstrap-spa) runtime shim for Sobs.
 *
 * This file is a LOCAL SHIM that will be replaced by the canonical
 * `abartrim/jinja-bootstrap-spa` package once that package becomes accessible
 * in the CI environment.  See docs/JBS_MIGRATION_AUDIT.md for details.
 *
 * Supported data attributes
 * ──────────────────────────
 * data-jbs-component="<name>"
 *   Marks a DOM region as a JBS component.  The element is the swap target.
 *
 * data-jbs-src="<url>"
 *   Fragment URL to fetch when the component is refreshed.  The runtime sends
 *   the stored ETag in If-None-Match so the server can return 304.
 *
 * data-jbs-trigger="<selector>"  (on any element inside the component)
 *   CSS selector of an element that, when clicked, triggers a refresh of the
 *   nearest ancestor [data-jbs-component].
 *
 * data-jbs-poc-instrumentation="true"  (on component root)
 *   Enables POC visual instrumentation: swap flash, cache-hit flash,
 *   loading opacity.  Should be removed or disabled before production rollout.
 *
 * data-jbs-phase  (managed by runtime, do not set manually)
 *   Reflects component lifecycle: idle | loading | swapping | not-modified
 *
 * CSS classes managed by runtime
 * ───────────────────────────────
 * jbs-is-loading       – added while request is in flight
 * jbs-swap-pulse       – added (then removed) after a 200 swap
 * jbs-not-modified-pulse – added (then removed) after a 304 hit
 */

(function () {
  'use strict';

  // -------------------------------------------------------------------------
  // POC instrumentation CSS (injected once into <head>)
  // -------------------------------------------------------------------------
  var _INSTRUMENTATION_CSS = [
    /* Loading: dim the component while fetching */
    '[data-jbs-poc-instrumentation][data-jbs-phase="loading"] { opacity: 0.55; transition: opacity 0.15s ease; }',
    /* 200 swap: blue-green flash */
    '@keyframes jbs-swap-flash { 0%{background:rgba(13,202,240,0.35)} 100%{background:transparent} }',
    '.jbs-swap-pulse { animation: jbs-swap-flash 0.9s ease-out; }',
    /* 304 cache hit: amber flash */
    '@keyframes jbs-not-modified-flash { 0%{background:rgba(255,193,7,0.35)} 100%{background:transparent} }',
    '.jbs-not-modified-pulse { animation: jbs-not-modified-flash 0.9s ease-out; }',
  ].join('\n');

  var _cssInjected = false;
  function _ensureCssInjected() {
    if (_cssInjected) return;
    _cssInjected = true;
    var style = document.createElement('style');
    style.textContent = _INSTRUMENTATION_CSS;
    style.dataset.jbsRuntime = '1';
    document.head.appendChild(style);
  }

  // -------------------------------------------------------------------------
  // Post-swap hook registry
  // Pages register component-specific re-wiring callbacks here so the
  // generic runtime does not need page-specific knowledge.
  // TODO(framework): replace with a first-class callback API in the canonical
  // jinja-bootstrap-spa package once it is installable.
  // -------------------------------------------------------------------------
  var _postSwapHooks = Object.create(null); // { componentName: [fn, ...] }

  function _runPostSwapHooks(name, el) {
    var hooks = _postSwapHooks[name];
    if (!hooks) return;
    for (var i = 0; i < hooks.length; i++) {
      try { hooks[i](el); } catch (e) { console.warn('[jbs-runtime] post-swap hook error:', e); }
    }
  }

  // -------------------------------------------------------------------------
  // ETag store  (in-memory per component name)
  // -------------------------------------------------------------------------
  var _etags = Object.create(null);

  // -------------------------------------------------------------------------
  // Phase helpers
  // -------------------------------------------------------------------------
  function _setPhase(el, phase) {
    el.dataset.jbsPhase = phase;
  }

  function _addThenRemove(el, cls, durationMs) {
    el.classList.add(cls);
    setTimeout(function () { el.classList.remove(cls); }, durationMs);
  }

  // -------------------------------------------------------------------------
  // Core: fetch a fragment and (maybe) swap the component's innerHTML
  // -------------------------------------------------------------------------
  function _refreshComponent(el) {
    var src = el.dataset.jbsSrc;
    if (!src) return;

    var name = el.dataset.jbsComponent || '';
    var poc = el.dataset.jbsPocInstrumentation === 'true';

    _setPhase(el, 'loading');
    el.classList.add('jbs-is-loading');

    var headers = {};
    if (_etags[name]) {
      headers['If-None-Match'] = _etags[name];
    }

    fetch(src, { headers: headers, credentials: 'same-origin' })
      .then(function (resp) {
        var newEtag = resp.headers.get('ETag') || '';
        if (newEtag) _etags[name] = newEtag;

        if (resp.status === 304) {
          // Not modified – show cache-hit instrumentation
          _setPhase(el, 'not-modified');
          el.classList.remove('jbs-is-loading');
          if (poc) _addThenRemove(el, 'jbs-not-modified-pulse', 1000);
          setTimeout(function () { _setPhase(el, 'idle'); }, 1000);
          return;
        }

        if (!resp.ok) {
          el.classList.remove('jbs-is-loading');
          _setPhase(el, 'idle');
          console.warn('[jbs-runtime] Fragment fetch returned', resp.status, 'for', src);
          return;
        }

        resp.text().then(function (html) {
          _setPhase(el, 'swapping');
          el.classList.remove('jbs-is-loading');
          // NOTE: outerHTML replacement is used here for simplicity.
          // This is a known limitation of this shim – it causes all event listeners
          // to be lost and requires re-wiring.  It also invalidates any cached
          // references to `el` held by other code – callers must re-query the DOM
          // after a swap.  The canonical jinja-bootstrap-spa framework
          // implementation should use a DOM diffing / morphing strategy
          // (see framework backlog: missing primitive #1 – smart DOM swap).
          el.outerHTML = html;

          // Re-resolve the element reference after outerHTML replacement
          var updated = document.querySelector('[data-jbs-component="' + name + '"]');
          if (updated) {
            _setPhase(updated, 'idle');
            if (poc) _addThenRemove(updated, 'jbs-swap-pulse', 1000);
            // Re-wire triggers on the freshly inserted fragment
            _wireComponent(updated);
            // Re-run TZ rendering if available
            if (window.sobsTimezone && typeof window.sobsTimezone.renderAll === 'function') {
              window.sobsTimezone.renderAll();
            }
            // Invoke registered post-swap hooks for the component (page-specific wiring).
            // TODO(framework): replace with a generic callback registry once the
            // canonical jinja-bootstrap-spa package is available.
            _runPostSwapHooks(name, updated);
          }
        });
      })
      .catch(function (err) {
        el.classList.remove('jbs-is-loading');
        _setPhase(el, 'idle');
        console.error('[jbs-runtime] Fragment fetch error:', err);
      });
  }

  // -------------------------------------------------------------------------
  // Wire a refresh trigger inside (or outside) a component
  // -------------------------------------------------------------------------
  function _wireComponent(root) {
    // Triggers declared inside the component itself
    root.querySelectorAll('[data-jbs-trigger]').forEach(function (trigger) {
      // Avoid double-wiring
      if (trigger.dataset.jbsWired) return;
      trigger.dataset.jbsWired = '1';
      trigger.addEventListener('click', function (e) {
        e.preventDefault();
        _refreshComponent(root);
      });
    });
  }

  // -------------------------------------------------------------------------
  // Bootstrap: wire external triggers + all components on DOMContentLoaded
  // -------------------------------------------------------------------------
  function _bootstrap() {
    _ensureCssInjected();

    // Wire all [data-jbs-component] elements
    document.querySelectorAll('[data-jbs-component]').forEach(function (el) {
      _setPhase(el, 'idle');
      _wireComponent(el);
    });

    // External triggers (outside the component): [data-jbs-target-component]
    document.querySelectorAll('[data-jbs-target-component]').forEach(function (trigger) {
      if (trigger.dataset.jbsWired) return;
      trigger.dataset.jbsWired = '1';
      var targetName = trigger.dataset.jbsTargetComponent;
      trigger.addEventListener('click', function (e) {
        e.preventDefault();
        var target = document.querySelector('[data-jbs-component="' + targetName + '"]');
        if (target) _refreshComponent(target);
      });
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', _bootstrap);
  } else {
    _bootstrap();
  }

  // Expose a small public API for page scripts that need to trigger refreshes
  window.jbsRuntime = {
    /**
     * Programmatically refresh a named component.
     * @param {string} name  – value of data-jbs-component
     */
    refresh: function (name) {
      var el = document.querySelector('[data-jbs-component="' + name + '"]');
      if (el) _refreshComponent(el);
    },
    /**
     * Return the current phase of a named component.
     * @param {string} name
     * @returns {string}  idle | loading | swapping | not-modified | (unknown)
     */
    phase: function (name) {
      var el = document.querySelector('[data-jbs-component="' + name + '"]');
      return el ? (el.dataset.jbsPhase || 'idle') : 'unknown';
    },
    /**
     * Register a post-swap hook for a component.
     * The callback receives the newly-inserted component element after each swap.
     * Use this to re-wire page-specific event handlers without modifying the runtime.
     *
     * @param {string}   name  – value of data-jbs-component
     * @param {Function} fn    – callback(element) invoked after each successful swap
     */
    onSwap: function (name, fn) {
      if (!_postSwapHooks[name]) _postSwapHooks[name] = [];
      _postSwapHooks[name].push(fn);
    },
  };
}());
