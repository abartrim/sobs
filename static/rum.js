/**
 * SOBS RUM – lightweight Real User Monitoring script.
 *
 * Usage:
 *   <script src="http://YOUR_SOBS_HOST/static/rum.js"></script>
 *   <script>
 *     SOBS.init({ endpoint: 'http://YOUR_SOBS_HOST/v1/rum', appName: 'my-app' });
 *   </script>
 *
 * Collects:
 *  - Page views
 *  - Web Vitals (LCP, FID/INP, CLS, TTFB, FCP) via PerformanceObserver
 *  - JS errors and unhandled promise rejections
 *  - Navigation / resource timing summaries
 */

(function (global) {
  'use strict';

  var SOBS = {};
  var _cfg = {};
  var _session = null;
  var _consoleBuffer = [];
  var _breadcrumbBuffer = [];
  var _traceContext = { traceId: '', spanId: '' };
  var _visualContext = null;
  var _consoleTracked = false;
  var _breadcrumbsTracked = false;

  function _bufferLimit(key, fallbackValue) {
    var raw = _cfg && _cfg[key];
    var limit = typeof raw === 'number' ? raw : parseInt(raw, 10);
    return limit > 0 ? limit : fallbackValue;
  }

  function _truncate(value, maxLen) {
    var str = String(value == null ? '' : value);
    return str.length > maxLen ? str.slice(0, maxLen - 1) + '…' : str;
  }

  function _safeSerialize(value, maxLen) {
    if (value == null) return '';
    if (typeof value === 'string') return _truncate(value, maxLen);
    if (typeof value === 'number' || typeof value === 'boolean') return String(value);
    if (value instanceof Error) {
      return _truncate((value.name || 'Error') + ': ' + (value.message || ''), maxLen);
    }
    try {
      return _truncate(JSON.stringify(value), maxLen);
    } catch (e) {
      return _truncate(Object.prototype.toString.call(value), maxLen);
    }
  }

  function _cloneEntries(entries) {
    return entries.map(function (entry) {
      return JSON.parse(JSON.stringify(entry));
    });
  }

  function _pushBounded(buffer, entry, limit) {
    buffer.push(entry);
    if (buffer.length > limit) buffer.splice(0, buffer.length - limit);
  }

  function _nodeHint(node) {
    if (!node || !node.tagName) return '';
    var hint = String(node.tagName || '').toLowerCase();
    if (node.id) hint += '#' + _truncate(node.id, 40);
    if (node.name) hint += '[name="' + _truncate(node.name, 32) + '"]';
    var className = typeof node.className === 'string' ? node.className.trim() : '';
    if (className) {
      var firstClass = className.split(/\s+/)[0];
      if (firstClass) hint += '.' + _truncate(firstClass, 32);
    }
    return hint;
  }

  function _recordConsole(level, args) {
    _pushBounded(_consoleBuffer, {
      timestamp: _ts(),
      level: level,
      message: _truncate(
        Array.prototype.slice.call(args).map(function (item) {
          return _safeSerialize(item, 280);
        }).join(' '),
        400
      )
    }, _bufferLimit('consoleBufferSize', 10));
  }

  function _addBreadcrumb(category, message, data) {
    _pushBounded(_breadcrumbBuffer, {
      timestamp: _ts(),
      category: category,
      message: _truncate(message || '', 180),
      data: data || {}
    }, _bufferLimit('breadcrumbBufferSize', 25));
  }

  function _captureContext() {
    return {
      page: {
        title: document.title,
        visibilityState: document.visibilityState || '',
        viewport: global.innerWidth && global.innerHeight ? (global.innerWidth + 'x' + global.innerHeight) : ''
      },
      breadcrumbs: {
        console: _cloneEntries(_consoleBuffer),
        user: _cloneEntries(_breadcrumbBuffer)
      }
    };
  }

  function _copyObject(value) {
    if (!value || typeof value !== 'object') return null;
    try {
      return JSON.parse(JSON.stringify(value));
    } catch (e) {
      return null;
    }
  }

  function _clearExpiredVisualContext() {
    if (_visualContext && _visualContext.expiresAt && Date.now() > _visualContext.expiresAt) {
      _visualContext = null;
    }
  }

  function _peekVisualContext() {
    _clearExpiredVisualContext();
    return _visualContext;
  }

  function _consumeVisualContextIfNeeded(context) {
    if (context && context.consumeOnce !== false) {
      _visualContext = null;
    }
  }

  function _normalizeVisualContext(data) {
    if (!data || typeof data !== 'object') return null;
    var normalized = {
      artifact: _copyObject(data.artifact),
      replay: _copyObject(data.replay),
      consumeOnce: data.consumeOnce !== false,
      expiresAt: 0
    };
    var ttlMs = typeof data.ttlMs === 'number' ? data.ttlMs : parseInt(data.ttlMs, 10);
    if (ttlMs > 0) normalized.expiresAt = Date.now() + ttlMs;
    if (!normalized.artifact && !normalized.replay) return null;
    return normalized;
  }

  function _parseTraceParent(value) {
    var text = String(value || '').trim();
    var match = text.match(/^([\da-f]{2})-([\da-f]{32})-([\da-f]{16})-([\da-f]{2})$/i);
    if (!match) return null;
    return {
      version: match[1].toLowerCase(),
      traceId: match[2].toLowerCase(),
      spanId: match[3].toLowerCase(),
      traceFlags: match[4].toLowerCase()
    };
  }

  function _setTraceContextFromTraceParent(value) {
    var parsed = _parseTraceParent(value);
    if (!parsed) return false;
    _traceContext = {
      traceId: parsed.traceId,
      spanId: parsed.spanId,
      traceFlags: parsed.traceFlags,
      traceparent: value
    };
    return true;
  }

  function _detectTraceContext() {
    if (_cfg.traceId || _cfg.spanId) {
      _traceContext = {
        traceId: _cfg.traceId || '',
        spanId: _cfg.spanId || '',
        traceFlags: _cfg.traceFlags || '',
        traceparent: ''
      };
      return;
    }

    if (_cfg.traceparent && _setTraceContextFromTraceParent(_cfg.traceparent)) return;

    var meta = document.querySelector('meta[name="traceparent"]');
    if (meta && _setTraceContextFromTraceParent(meta.getAttribute('content'))) return;

    if (global.__SOBS_TRACEPARENT__ && _setTraceContextFromTraceParent(global.__SOBS_TRACEPARENT__)) return;
    if (global.__TRACEPARENT__ && _setTraceContextFromTraceParent(global.__TRACEPARENT__)) return;
  }

  function _mergeErrorContext(payload) {
    var merged = Object.assign({}, payload, _captureContext());
    var visual = _peekVisualContext();
    if (visual) {
      if (!merged.artifact && visual.artifact) merged.artifact = _copyObject(visual.artifact);
      if (!merged.replay && visual.replay) merged.replay = _copyObject(visual.replay);
      _consumeVisualContextIfNeeded(visual);
    }
    return merged;
  }

  function _applyTraceContext(payload) {
    if (_traceContext.traceId && !payload.traceId) payload.traceId = _traceContext.traceId;
    if (_traceContext.spanId && !payload.spanId) payload.spanId = _traceContext.spanId;
    if (_traceContext.traceFlags && !payload.traceFlags) payload.traceFlags = _traceContext.traceFlags;
    if (_traceContext.traceparent && !payload.traceparent) payload.traceparent = _traceContext.traceparent;
    return payload;
  }

  // ----- session id -----
  function _getSession() {
    try {
      var k = 'sobs_sid';
      var sid = sessionStorage.getItem(k);
      if (!sid) {
        sid = _uuid();
        sessionStorage.setItem(k, sid);
      }
      return sid;
    } catch (e) {
      return _uuid();
    }
  }

  function _uuid() {
    return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, function (c) {
      var r = (Math.random() * 16) | 0;
      return (c === 'x' ? r : (r & 0x3) | 0x8).toString(16);
    });
  }

  // ----- send -----
  function _send(events) {
    if (!_cfg.endpoint) return;
    var payload = Array.isArray(events) ? events : [events];
    payload = payload.map(function (e) {
      return _applyTraceContext(Object.assign({ sessionId: _session, appName: _cfg.appName || '' }, e));
    });
    try {
      navigator.sendBeacon(_cfg.endpoint, JSON.stringify(payload));
    } catch (e) {
      // fallback
      var xhr = new XMLHttpRequest();
      xhr.open('POST', _cfg.endpoint, true);
      xhr.setRequestHeader('Content-Type', 'application/json');
      xhr.send(JSON.stringify(payload));
    }
  }

  function _ts() {
    return new Date().toISOString();
  }

  // ----- page view -----
  function _trackPageView() {
    _send({
      type: 'pageview',
      timestamp: _ts(),
      url: location.href,
      title: document.title,
      referrer: document.referrer,
    });
  }

  function _trackConsole() {
    if (_consoleTracked) return;
    _consoleTracked = true;
    ['warn', 'error'].forEach(function (level) {
      if (!global.console || typeof global.console[level] !== 'function') return;
      var original = global.console[level];
      global.console[level] = function () {
        try {
          _recordConsole(level, arguments);
        } catch (e) {}
        return original.apply(this, arguments);
      };
    });
  }

  function _trackBreadcrumbs() {
    if (_breadcrumbsTracked) return;
    _breadcrumbsTracked = true;
    global.addEventListener('click', function (evt) {
      var hint = _nodeHint(evt.target);
      if (!hint) return;
      _addBreadcrumb('ui.click', 'Clicked ' + hint, { target: hint });
    }, true);

    global.addEventListener('submit', function (evt) {
      var hint = _nodeHint(evt.target);
      _addBreadcrumb('ui.submit', hint ? 'Submitted ' + hint : 'Submitted form', { target: hint });
    }, true);

    global.addEventListener('visibilitychange', function () {
      _addBreadcrumb('ui.visibility', 'Visibility changed', { state: document.visibilityState || '' });
    });

    if (global.fetch) {
      var originalFetch = global.fetch;
      global.fetch = function () {
        var startedAt = Date.now();
        var requestUrl = arguments[0] && arguments[0].url ? arguments[0].url : arguments[0];
        return originalFetch.apply(this, arguments).then(function (response) {
          if (!response.ok) {
            _addBreadcrumb('http.fetch', 'Fetch failed', {
              url: _truncate(String(requestUrl || ''), 200),
              status: response.status,
              durationMs: Date.now() - startedAt
            });
          }
          return response;
        }).catch(function (error) {
          _addBreadcrumb('http.fetch', 'Fetch exception', {
            url: _truncate(String(requestUrl || ''), 200),
            durationMs: Date.now() - startedAt,
            error: _safeSerialize(error, 180)
          });
          throw error;
        });
      };
    }
  }

  // ----- errors -----
  function _trackErrors() {
    global.addEventListener('error', function (evt) {
      var target = evt.target || evt.srcElement;
      if (target && target !== global) {
        _send(_mergeErrorContext({
          type: 'error',
          timestamp: _ts(),
          url: location.href,
          message: 'Failed to load ' + (_nodeHint(target) || 'resource'),
          errorType: 'ResourceError',
          errorSource: 'resource-error',
          filename: target.currentSrc || target.src || target.href || '',
          target: _nodeHint(target),
        }));
        return;
      }
      _send(_mergeErrorContext({
        type: 'error',
        timestamp: _ts(),
        url: location.href,
        message: evt.message,
        errorType: (evt.error && evt.error.name) || 'Error',
        stack: (evt.error && evt.error.stack) || '',
        errorSource: 'window.onerror',
        filename: evt.filename,
        lineno: evt.lineno,
        colno: evt.colno,
      }));
    }, true);
    global.addEventListener('unhandledrejection', function (evt) {
      var reason = evt.reason || {};
      _send(_mergeErrorContext({
        type: 'unhandledrejection',
        timestamp: _ts(),
        url: location.href,
        message: reason.message || String(reason),
        errorType: (reason.name) || 'UnhandledRejection',
        stack: reason.stack || '',
        errorSource: 'unhandledrejection',
      }));
    });
  }

  // ----- Web Vitals via PerformanceObserver -----
  function _reportWebVital(name, value, rating) {
    _send({
      type: 'web-vital',
      timestamp: _ts(),
      url: location.href,
      name: name,
      value: value,
      rating: rating || 'unknown',
    });
  }

  function _rating(name, value) {
    var thresholds = {
      LCP:  [2500, 4000],
      FID:  [100,  300],
      INP:  [200,  500],
      CLS:  [0.1,  0.25],
      TTFB: [800,  1800],
      FCP:  [1800, 3000],
    };
    var t = thresholds[name];
    if (!t) return 'unknown';
    if (value <= t[0]) return 'good';
    if (value <= t[1]) return 'needs-improvement';
    return 'poor';
  }

  function _trackWebVitals() {
    if (!global.PerformanceObserver) return;

    // LCP
    try {
      new PerformanceObserver(function (list) {
        var entries = list.getEntries();
        var last = entries[entries.length - 1];
        if (last) _reportWebVital('LCP', Math.round(last.startTime), _rating('LCP', last.startTime));
      }).observe({ type: 'largest-contentful-paint', buffered: true });
    } catch (e) {}

    // FID / INP
    try {
      new PerformanceObserver(function (list) {
        list.getEntries().forEach(function (entry) {
          var name = entry.interactionId ? 'INP' : 'FID';
          var val = entry.processingStart - entry.startTime;
          _reportWebVital(name, Math.round(val), _rating(name, val));
        });
      }).observe({ type: 'first-input', buffered: true });
    } catch (e) {}

    // CLS
    try {
      var clsVal = 0;
      new PerformanceObserver(function (list) {
        list.getEntries().forEach(function (entry) {
          if (!entry.hadRecentInput) clsVal += entry.value;
        });
      }).observe({ type: 'layout-shift', buffered: true });
      global.addEventListener('visibilitychange', function () {
        if (document.visibilityState === 'hidden')
          _reportWebVital('CLS', Math.round(clsVal * 1000) / 1000, _rating('CLS', clsVal));
      });
    } catch (e) {}

    // Navigation timing: TTFB + FCP
    try {
      new PerformanceObserver(function (list) {
        list.getEntries().forEach(function (entry) {
          if (entry.name === 'first-contentful-paint')
            _reportWebVital('FCP', Math.round(entry.startTime), _rating('FCP', entry.startTime));
        });
      }).observe({ type: 'paint', buffered: true });
    } catch (e) {}

    global.addEventListener('load', function () {
      try {
        var nav = performance.getEntriesByType('navigation')[0];
        if (nav && nav.responseStart) {
          var ttfb = nav.responseStart - nav.requestStart;
          _reportWebVital('TTFB', Math.round(ttfb), _rating('TTFB', ttfb));
        }
      } catch (e) {}
    });
  }

  // ----- SPA navigation -----
  function _trackSPANavigation() {
    var origPush = history.pushState;
    var origReplace = history.replaceState;
    function onNav(kind) {
      _addBreadcrumb('navigation', kind || 'history', { url: location.href });
      setTimeout(_trackPageView, 0);
    }
    history.pushState = function () { origPush.apply(this, arguments); onNav('pushState'); };
    history.replaceState = function () { origReplace.apply(this, arguments); onNav('replaceState'); };
    global.addEventListener('popstate', function () { onNav('popstate'); });
  }

  // ----- public API -----
  SOBS.init = function (cfg) {
    _cfg = cfg || {};
    _session = _getSession();
    _detectTraceContext();
    _trackConsole();
    _trackBreadcrumbs();
    _trackPageView();
    _trackErrors();
    _trackWebVitals();
    if (_cfg.trackSPA !== false) _trackSPANavigation();
  };

  SOBS.track = function (eventType, data) {
    _send(Object.assign({ type: eventType, timestamp: _ts(), url: location.href }, data || {}));
  };

  SOBS.captureException = function (error, data) {
    var payload = Object.assign({}, data || {});
    var err = error || {};
    _send(_mergeErrorContext(Object.assign({
      type: payload.type || 'error',
      timestamp: _ts(),
      url: location.href,
      message: payload.message || err.message || String(error || 'Unknown browser error'),
      errorType: payload.errorType || err.name || 'Error',
      stack: payload.stack || err.stack || '',
      errorSource: payload.errorSource || 'captureException'
    }, payload)));
  };

  SOBS.addBreadcrumb = function (category, message, data) {
    _addBreadcrumb(category, message, data);
  };

  SOBS.setVisualContext = function (data) {
    _visualContext = _normalizeVisualContext(data);
    return !!_visualContext;
  };

  SOBS.setReplayContext = function (replay, options) {
    var current = _peekVisualContext() || {};
    return SOBS.setVisualContext({
      artifact: _copyObject(current.artifact),
      replay: _copyObject(replay),
      ttlMs: options && options.ttlMs,
      consumeOnce: options ? options.consumeOnce : undefined
    });
  };

  SOBS.setArtifactContext = function (artifact, options) {
    var current = _peekVisualContext() || {};
    return SOBS.setVisualContext({
      artifact: _copyObject(artifact),
      replay: _copyObject(current.replay),
      ttlMs: options && options.ttlMs,
      consumeOnce: options ? options.consumeOnce : undefined
    });
  };

  SOBS.clearVisualContext = function () {
    _visualContext = null;
  };

  SOBS.setTraceContext = function (traceId, spanId) {
    _traceContext = {
      traceId: traceId || '',
      spanId: spanId || '',
      traceFlags: _traceContext.traceFlags || '',
      traceparent: _traceContext.traceparent || ''
    };
  };

  SOBS.setTraceParent = function (traceparent) {
    return _setTraceContextFromTraceParent(traceparent);
  };

  global.SOBS = SOBS;
})(window);
