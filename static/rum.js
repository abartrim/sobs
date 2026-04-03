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
  var _replayEvents = [];
  var _replayScriptPromise = null;
  var _replayStopFn = null;
  var _replayRecorderStarted = false;
  var _screenshotScriptPromise = null;
  var _isInitialized = false;

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

  function _extractConsoleErrorDetails(items) {
    var details = {
      errorType: '',
      stack: '',
      source: ''
    };

    for (var i = 0; i < items.length; i += 1) {
      var item = items[i];
      if (!item) continue;

      if (item instanceof Error || (typeof item === 'object' && (item.stack || item.message))) {
        var filename = item.fileName || item.filename || '';
        var line = item.lineNumber || item.lineno || '';
        var col = item.columnNumber || item.colno || '';

        if (!details.errorType && item.name) details.errorType = _truncate(String(item.name), 80);
        if (!details.stack && item.stack) details.stack = _truncate(String(item.stack), 2400);

        if (!details.source && filename) {
          details.source = _truncate(
            String(filename) + (line ? (':' + String(line)) : '') + (col ? (':' + String(col)) : ''),
            240
          );
        }
      }
    }

    if (!details.source && details.stack) {
      var firstFrame = String(details.stack).split('\n')[1] || '';
      details.source = _truncate(firstFrame.replace(/^\s*at\s+/, ''), 240);
    }

    return details;
  }

  function _recordConsole(level, args) {
    var items = Array.prototype.slice.call(args);
    var details = _extractConsoleErrorDetails(items);
    _pushBounded(_consoleBuffer, {
      timestamp: _ts(),
      level: level,
      message: _truncate(
        items.map(function (item) {
          return _safeSerialize(item, 280);
        }).join(' '),
        400
      ),
      errorType: details.errorType,
      source: details.source,
      stack: details.stack
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

  function _replayConfig() {
    return (_cfg && _cfg.replay && typeof _cfg.replay === 'object') ? _cfg.replay : null;
  }

  function _cloneSafe(value) {
    try {
      return JSON.parse(JSON.stringify(value));
    } catch (e) {
      return null;
    }
  }

  function _replayEventLimit() {
    var replay = _replayConfig();
    if (!replay) return 400;
    var raw = replay.maxEvents;
    var n = typeof raw === 'number' ? raw : parseInt(raw, 10);
    return n > 10 ? n : 400;
  }

  function _recordReplayEvent(event) {
    var safe = _cloneSafe(event);
    if (!safe) return;
    _pushBounded(_replayEvents, safe, _replayEventLimit());
  }

  function _getReplayRecorderFactory() {
    if (typeof global.rrwebRecord === 'function') return global.rrwebRecord;
    if (global.rrweb && typeof global.rrweb.record === 'function') return global.rrweb.record;
    return null;
  }

  function _loadReplayScript(scriptUrl) {
    if (_replayScriptPromise) return _replayScriptPromise;
    _replayScriptPromise = new Promise(function (resolve, reject) {
      var script = document.createElement('script');
      script.async = true;
      script.src = scriptUrl;
      script.onload = function () { resolve(true); };
      script.onerror = function () { reject(new Error('Failed to load rrweb script')); };
      document.head.appendChild(script);
    });
    return _replayScriptPromise;
  }

  function _screenshotConfig() {
    var replay = _replayConfig();
    if (!replay || !replay.screenshot || typeof replay.screenshot !== 'object') return null;
    return replay.screenshot;
  }

  function _getScreenshotFactory() {
    if (typeof global.html2canvas === 'function') return global.html2canvas;
    return null;
  }

  function _loadScreenshotScript(scriptUrl) {
    if (_screenshotScriptPromise) return _screenshotScriptPromise;
    _screenshotScriptPromise = new Promise(function (resolve, reject) {
      var script = document.createElement('script');
      script.async = true;
      script.src = scriptUrl;
      script.onload = function () { resolve(true); };
      script.onerror = function () { reject(new Error('Failed to load screenshot script')); };
      document.head.appendChild(script);
    });
    return _screenshotScriptPromise;
  }

  function _resizeCanvas(source, maxEdge) {
    if (!maxEdge || maxEdge < 256) return source;
    var width = source.width || 0;
    var height = source.height || 0;
    var currentMax = Math.max(width, height);
    if (!currentMax || currentMax <= maxEdge) return source;
    var ratio = maxEdge / currentMax;
    var dst = document.createElement('canvas');
    dst.width = Math.max(1, Math.round(width * ratio));
    dst.height = Math.max(1, Math.round(height * ratio));
    var ctx = dst.getContext('2d');
    if (!ctx) return source;
    ctx.drawImage(source, 0, 0, dst.width, dst.height);
    return dst;
  }

  function _captureScreenshotForError(errorPayload) {
    if (errorPayload && errorPayload.artifact) return Promise.resolve(null);

    var screenshot = _screenshotConfig();
    if (!screenshot || screenshot.enabled !== true) return Promise.resolve(null);

    function capture() {
      var screenshotFn = _getScreenshotFactory();
      if (!screenshotFn) return Promise.resolve(null);

      var target = document.body || document.documentElement;
      var options = Object.assign({
        logging: false,
        useCORS: true,
        backgroundColor: null,
        scale: screenshot.scale || 0.7
      }, screenshot.options || {});

      return Promise.resolve(screenshotFn(target, options)).then(function (canvas) {
        if (!canvas || !canvas.toDataURL) return null;
        var resized = _resizeCanvas(canvas, screenshot.maxEdge || 1400);
        var mimeType = screenshot.mimeType || 'image/jpeg';
        var quality = typeof screenshot.quality === 'number' ? screenshot.quality : 0.75;
        var dataUrl = resized.toDataURL(mimeType, quality);
        return {
          dataUrl: dataUrl,
          mimeType: mimeType,
          width: resized.width || 0,
          height: resized.height || 0,
          source: 'html2canvas'
        };
      }).catch(function () {
        return null;
      });
    }

    if (_getScreenshotFactory()) return capture();
    if (!screenshot.scriptUrl) return Promise.resolve(null);
    return _loadScreenshotScript(screenshot.scriptUrl).then(function () {
      return capture();
    }).catch(function () {
      return null;
    });
  }

  function _startReplayRecorder() {
    if (_replayRecorderStarted) return;
    var replay = _replayConfig();
    if (!replay || replay.enabled !== true) return;

    function bootRecorder() {
      var recordFn = _getReplayRecorderFactory();
      if (!recordFn) return;
      try {
        _replayStopFn = recordFn({
          emit: _recordReplayEvent,
          sampling: replay.sampling || {
            mousemove: false,
            scroll: 150,
            input: 'last'
          }
        });
        _replayRecorderStarted = true;
      } catch (e) {}
    }

    if (_getReplayRecorderFactory()) {
      bootRecorder();
      return;
    }

    if (!replay.scriptUrl) return;
    _loadReplayScript(replay.scriptUrl).then(function () {
      bootRecorder();
    }).catch(function () {});
  }

  function _buildReplayEnvelope(errorPayload, screenshot) {
    var envelope = {
      provider: 'rrweb',
      events: _cloneEntries(_replayEvents),
      error: {
        type: errorPayload.type,
        message: errorPayload.message,
        errorType: errorPayload.errorType,
        errorSource: errorPayload.errorSource,
        timestamp: errorPayload.timestamp,
        url: errorPayload.url
      }
    };
    if (screenshot) envelope.screenshot = screenshot;
    return envelope;
  }

  function _attachReplayArtifacts(errorPayload) {
    var replay = _replayConfig();
    if (!replay || replay.enabled !== true) return Promise.resolve(errorPayload);

    var uploader = replay.upload;
    if (typeof uploader !== 'function') {
      if (_replayEvents.length && !errorPayload.replay) {
        errorPayload.replay = {
          provider: 'rrweb',
          eventCount: _replayEvents.length
        };
      }
      return Promise.resolve(errorPayload);
    }

    return _captureScreenshotForError(errorPayload).then(function (screenshot) {
      var envelope = _buildReplayEnvelope(errorPayload, screenshot);
      return uploader(envelope);
    }).then(function (result) {
      if (result && typeof result === 'object') {
        if (result.replay && !errorPayload.replay) errorPayload.replay = result.replay;
        if (result.artifact && !errorPayload.artifact) errorPayload.artifact = result.artifact;
      }
      return errorPayload;
    }).catch(function () {
      return errorPayload;
    });
  }

  function _emitErrorEvent(payload) {
    _attachReplayArtifacts(payload).then(function (enriched) {
      _send(_mergeErrorContext(enriched));
    });
  }

  function _randomHex(bytes) {
    var out = '';
    var len = bytes * 2;
    try {
      if (global.crypto && typeof global.crypto.getRandomValues === 'function') {
        var arr = new Uint8Array(bytes);
        global.crypto.getRandomValues(arr);
        for (var i = 0; i < arr.length; i += 1) {
          out += ('0' + arr[i].toString(16)).slice(-2);
        }
        return out;
      }
    } catch (e) {}

    while (out.length < len) {
      out += ('0' + ((Math.random() * 256) | 0).toString(16)).slice(-2);
    }
    return out.slice(0, len);
  }

  function _isHex(value, len) {
    return new RegExp('^[0-9a-f]{' + String(len) + '}$', 'i').test(String(value || ''));
  }

  function _newTraceId() {
    return _randomHex(16);
  }

  function _newSpanId() {
    return _randomHex(8);
  }

  function _formatTraceParent(traceId, spanId, flags) {
    var tid = String(traceId || '').toLowerCase();
    var sid = String(spanId || '').toLowerCase();
    var flg = String(flags || '01').toLowerCase();
    if (!_isHex(tid, 32) || !_isHex(sid, 16) || !_isHex(flg, 2)) return '';
    return '00-' + tid + '-' + sid + '-' + flg;
  }

  function _ensureTraceContext() {
    if (!_isHex(_traceContext.traceId, 32)) _traceContext.traceId = _newTraceId();
    if (!_isHex(_traceContext.spanId, 16)) _traceContext.spanId = _newSpanId();
    if (!_isHex(_traceContext.traceFlags, 2)) _traceContext.traceFlags = '01';
    _traceContext.traceparent = _formatTraceParent(
      _traceContext.traceId,
      _traceContext.spanId,
      _traceContext.traceFlags
    );
    return _traceContext;
  }

  function _nextOutboundTraceContext() {
    var base = _ensureTraceContext();
    var childSpanId = _newSpanId();
    return {
      traceId: base.traceId,
      spanId: childSpanId,
      traceFlags: base.traceFlags,
      traceparent: _formatTraceParent(base.traceId, childSpanId, base.traceFlags)
    };
  }

  function _shouldPropagateTraceToUrl(value) {
    if (!value) return false;
    try {
      var parsed = new URL(String(value), location.href);
      if (parsed.origin === location.origin) return true;
      if (_cfg && _cfg.tracePropagationCrossOrigin === true) return true;
      var allowed = (_cfg && _cfg.tracePropagationOrigins) || [];
      if (Array.isArray(allowed) && allowed.indexOf(parsed.origin) !== -1) return true;
    } catch (e) {}
    return false;
  }

  function _injectTraceIntoFetchArgs(args) {
    var request = args[0];
    var init = args.length > 1 ? args[1] : undefined;
    var requestUrl = request && request.url ? request.url : request;

    if (!_shouldPropagateTraceToUrl(requestUrl)) {
      return { args: args, requestUrl: requestUrl, traceContext: null };
    }

    var traceCtx = _nextOutboundTraceContext();
    if (!traceCtx.traceparent) {
      return { args: args, requestUrl: requestUrl, traceContext: null };
    }

    if (typeof Request !== 'undefined' && request instanceof Request) {
      var reqHeaders = new Headers(request.headers || {});
      if (!reqHeaders.has('traceparent')) reqHeaders.set('traceparent', traceCtx.traceparent);
      if (_cfg.tracestate && !reqHeaders.has('tracestate')) reqHeaders.set('tracestate', String(_cfg.tracestate));
      var reqInit = Object.assign({}, init || {}, { headers: reqHeaders });
      return {
        args: [new Request(request, reqInit)],
        requestUrl: requestUrl,
        traceContext: traceCtx
      };
    }

    var headers = new Headers((init && init.headers) || {});
    if (!headers.has('traceparent')) headers.set('traceparent', traceCtx.traceparent);
    if (_cfg.tracestate && !headers.has('tracestate')) headers.set('tracestate', String(_cfg.tracestate));
    var nextInit = Object.assign({}, init || {}, { headers: headers });
    return {
      args: [request, nextInit],
      requestUrl: requestUrl,
      traceContext: traceCtx
    };
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
      traceparent: _formatTraceParent(parsed.traceId, parsed.spanId, parsed.traceFlags)
    };
    return true;
  }

  function _detectTraceContext() {
    if (_cfg.traceId || _cfg.spanId) {
      var cfgTraceId = _cfg.traceId || '';
      var cfgSpanId = _cfg.spanId || '';
      var cfgFlags = _cfg.traceFlags || '';
      _traceContext = {
        traceId: cfgTraceId,
        spanId: cfgSpanId,
        traceFlags: cfgFlags,
        traceparent: _formatTraceParent(cfgTraceId, cfgSpanId, cfgFlags)
      };
      _ensureTraceContext();
      return;
    }

    if (_cfg.traceparent && _setTraceContextFromTraceParent(_cfg.traceparent)) {
      _ensureTraceContext();
      return;
    }

    var meta = document.querySelector('meta[name="traceparent"]');
    if (meta && _setTraceContextFromTraceParent(meta.getAttribute('content'))) {
      _ensureTraceContext();
      return;
    }

    if (global.__SOBS_TRACEPARENT__ && _setTraceContextFromTraceParent(global.__SOBS_TRACEPARENT__)) {
      _ensureTraceContext();
      return;
    }
    if (global.__TRACEPARENT__ && _setTraceContextFromTraceParent(global.__TRACEPARENT__)) {
      _ensureTraceContext();
      return;
    }

    // Fall back to a generated client trace context so all RUM events can correlate.
    _ensureTraceContext();
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
    _ensureTraceContext();
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
      var item = _applyTraceContext(Object.assign({ sessionId: _session, appName: _cfg.appName || '' }, e));
      if (_cfg.clientAuthToken && !item.clientAuthToken) item.clientAuthToken = _cfg.clientAuthToken;
      return item;
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
        var rawArgs = Array.prototype.slice.call(arguments);
        var wrapped = _injectTraceIntoFetchArgs(rawArgs);
        var requestUrl = wrapped.requestUrl;
        return originalFetch.apply(this, wrapped.args).then(function (response) {
          if (!response.ok) {
            _addBreadcrumb('http.fetch', 'Fetch failed', {
              url: _truncate(String(requestUrl || ''), 200),
              status: response.status,
              durationMs: Date.now() - startedAt,
              traceId: wrapped.traceContext ? wrapped.traceContext.traceId : '',
              spanId: wrapped.traceContext ? wrapped.traceContext.spanId : ''
            });
          }
          return response;
        }).catch(function (error) {
          _addBreadcrumb('http.fetch', 'Fetch exception', {
            url: _truncate(String(requestUrl || ''), 200),
            durationMs: Date.now() - startedAt,
            traceId: wrapped.traceContext ? wrapped.traceContext.traceId : '',
            spanId: wrapped.traceContext ? wrapped.traceContext.spanId : '',
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
        _emitErrorEvent({
          type: 'error',
          timestamp: _ts(),
          url: location.href,
          message: 'Failed to load ' + (_nodeHint(target) || 'resource'),
          errorType: 'ResourceError',
          errorSource: 'resource-error',
          filename: target.currentSrc || target.src || target.href || '',
          target: _nodeHint(target),
        });
        return;
      }
      _emitErrorEvent({
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
      });
    }, true);
    global.addEventListener('unhandledrejection', function (evt) {
      var reason = evt.reason || {};
      _emitErrorEvent({
        type: 'unhandledrejection',
        timestamp: _ts(),
        url: location.href,
        message: reason.message || String(reason),
        errorType: (reason.name) || 'UnhandledRejection',
        stack: reason.stack || '',
        errorSource: 'unhandledrejection',
      });
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
  function _bootWithConfig() {
    if (_isInitialized) return;
    _session = _getSession();
    _detectTraceContext();
    _startReplayRecorder();
    _trackConsole();
    _trackBreadcrumbs();
    _trackPageView();
    _trackErrors();
    _trackWebVitals();
    if (_cfg.trackSPA !== false) _trackSPANavigation();
    _isInitialized = true;
  }

  function _scriptUrlParams(scriptEl) {
    if (!scriptEl || !scriptEl.src) return null;
    try {
      return new URL(scriptEl.src, location.href);
    } catch (e) {
      return null;
    }
  }

  function _scriptElement() {
    if (document.currentScript && document.currentScript.tagName === 'SCRIPT') return document.currentScript;
    var scripts = document.getElementsByTagName('script');
    for (var i = scripts.length - 1; i >= 0; i -= 1) {
      var src = scripts[i].getAttribute('src') || '';
      if (src.indexOf('/static/rum.js') !== -1 || src.indexOf('rum.js') !== -1) return scripts[i];
    }
    return null;
  }

  function _readScriptAutoConfig() {
    var script = _scriptElement();
    if (!script) return null;
    var ds = script.dataset || {};
    var parsed = _scriptUrlParams(script);
    var params = parsed ? parsed.searchParams : null;

    var appName = ds.sobsApp || (params && (params.get('app') || params.get('appName'))) || '';
    var endpoint = ds.sobsEndpoint || (params && params.get('endpoint')) || '';
    var token = ds.sobsClientToken || (params && params.get('clientToken')) || '';
    var tokenUrl = ds.sobsClientTokenUrl || (params && params.get('clientTokenUrl')) || '';
    var autoRaw = ds.sobsAuto || (params && params.get('auto')) || '';
    var explicitAuto = String(autoRaw || '').trim();
    var autoEnabled = explicitAuto ? (explicitAuto.toLowerCase() !== 'false') : true;
    var hasExplicitConfig = !!(ds.sobsApp || ds.sobsEndpoint || ds.sobsClientToken || ds.sobsClientTokenUrl);
    var hasQueryConfig = !!(params && (params.get('app') || params.get('appName') || params.get('endpoint') || params.get('clientToken') || params.get('clientTokenUrl')));

    if (!endpoint && parsed && parsed.origin) endpoint = parsed.origin + '/v1/rum';
    // Avoid surprising auto-init when script is included without explicit config.
    if (!explicitAuto && !hasExplicitConfig && !hasQueryConfig) return null;
    if (!autoEnabled || (!endpoint && !appName)) return null;

    return {
      endpoint: endpoint,
      appName: appName,
      clientAuthToken: token,
      clientAuthTokenUrl: tokenUrl
    };
  }

  function _fetchClientToken(url, appName) {
    if (!url || !global.fetch) return Promise.resolve('');
    return fetch(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ appName: appName || '', origin: location.origin })
    }).then(function (resp) {
      if (!resp.ok) throw new Error('client token fetch failed');
      return resp.json();
    }).then(function (payload) {
      return payload && payload.token ? String(payload.token) : '';
    }).catch(function () {
      return '';
    });
  }

  SOBS.init = function (cfg) {
    _cfg = cfg || {};
    if (_isInitialized) {
      // Allow late init calls (e.g. after plain script include) to enable replay/token config.
      _startReplayRecorder();
      return Promise.resolve(true);
    }
    if (_cfg.clientAuthToken) {
      _bootWithConfig();
      return Promise.resolve(true);
    }
    if (_cfg.clientAuthTokenUrl) {
      return _fetchClientToken(_cfg.clientAuthTokenUrl, _cfg.appName).then(function (token) {
        if (token) _cfg.clientAuthToken = token;
        _bootWithConfig();
        return true;
      });
    }
    _bootWithConfig();
    return Promise.resolve(true);
  };

  SOBS.track = function (eventType, data) {
    _send(Object.assign({ type: eventType, timestamp: _ts(), url: location.href }, data || {}));
  };

  SOBS.captureException = function (error, data) {
    var payload = Object.assign({}, data || {});
    var err = error || {};
    _emitErrorEvent(Object.assign({
      type: payload.type || 'error',
      timestamp: _ts(),
      url: location.href,
      message: payload.message || err.message || String(error || 'Unknown browser error'),
      errorType: payload.errorType || err.name || 'Error',
      stack: payload.stack || err.stack || '',
      errorSource: payload.errorSource || 'captureException'
    }, payload));
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
    var normalizedTraceId = _isHex(traceId, 32) ? String(traceId).toLowerCase() : _newTraceId();
    var normalizedSpanId = _isHex(spanId, 16) ? String(spanId).toLowerCase() : _newSpanId();
    var flags = _isHex(_traceContext.traceFlags, 2) ? _traceContext.traceFlags : '01';
    _traceContext = {
      traceId: normalizedTraceId,
      spanId: normalizedSpanId,
      traceFlags: flags,
      traceparent: _formatTraceParent(normalizedTraceId, normalizedSpanId, flags)
    };
  };

  SOBS.setTraceParent = function (traceparent) {
    return _setTraceContextFromTraceParent(traceparent);
  };

  SOBS.setReplayUpload = function (uploader) {
    _cfg.replay = _cfg.replay || {};
    _cfg.replay.upload = uploader;
  };

  SOBS.enableReplay = function (options) {
    _cfg.replay = Object.assign({}, _cfg.replay || {}, options || {}, { enabled: true });
    _startReplayRecorder();
  };

  SOBS.disableReplay = function () {
    _cfg.replay = Object.assign({}, _cfg.replay || {}, { enabled: false });
    if (typeof _replayStopFn === 'function') {
      try { _replayStopFn(); } catch (e) {}
    }
    _replayStopFn = null;
    _replayRecorderStarted = false;
  };

  SOBS.setClientAuthToken = function (token) {
    _cfg.clientAuthToken = token ? String(token) : '';
  };

  global.SOBS = SOBS;

  // Auto-init for one-script usage:
  // <script src="https://SOBS/static/rum.js?app=my-app"></script>
  // or data-sobs-* attributes on the script tag.
  var autoCfg = _readScriptAutoConfig();
  if (autoCfg && !global.__SOBS_AUTO_INIT_DONE__) {
    global.__SOBS_AUTO_INIT_DONE__ = true;
    SOBS.init(autoCfg);
  }
})(window);
