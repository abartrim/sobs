/*
 * SOBS DOM masking helper for screenshot workflows.
 *
 * Usage:
 *   const session = await window.SOBSDomMasking.sanitizeDomForScreenshot({
 *     rulesUrl: '/api/settings/masking/rules'
 *   });
 *   try {
 *     // capture + upload screenshot bytes
 *   } finally {
 *     session.restore();
 *   }
 */
(function (global) {
  'use strict';

  var DEFAULT_REDACTION = '****';

  function toStringValue(value) {
    if (value === null || value === undefined) return '';
    return String(value);
  }

  function normalizeFlags(flagText) {
    var out = '';
    if (!flagText) return out;
    for (var i = 0; i < flagText.length; i += 1) {
      var ch = flagText.charAt(i);
      if ('gimsuy'.indexOf(ch) !== -1 && out.indexOf(ch) === -1) out += ch;
    }
    return out;
  }

  function compilePattern(rawPattern) {
    if (!rawPattern || typeof rawPattern !== 'string') return null;

    var pattern = rawPattern;
    var flags = 'g';

    // Support leading Python-style inline flags like (?i) or (?is).
    var inlineMatch = pattern.match(/^\(\?([a-zA-Z]+)\)/);
    if (inlineMatch) {
      flags += normalizeFlags(inlineMatch[1]);
      pattern = pattern.slice(inlineMatch[0].length);
    }

    // Best-effort Python-to-JS compatibility adjustments.
    pattern = pattern.replace(/\\A/g, '^').replace(/\\Z/g, '$');
    pattern = pattern.replace(/\(\?P<[^>]+>/g, '(');

    try {
      return new RegExp(pattern, normalizeFlags(flags));
    } catch (_err) {
      return null;
    }
  }

  function escapeRegExp(value) {
    return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  }

  function buildKeyRegexes(keys) {
    var out = [];
    if (!Array.isArray(keys)) return out;

    for (var i = 0; i < keys.length; i += 1) {
      var key = toStringValue(keys[i]).trim();
      if (!key) continue;

      var escaped = escapeRegExp(key);
      out.push({
        // JSON-like: "password": "value"
        regex: new RegExp('("' + escaped + '"\\s*:\\s*)("[^"\\n\\r]*"|[^,}\\n\\r]+)', 'gi'),
        replacement: '$1"' + DEFAULT_REDACTION + '"'
      });
      out.push({
        // key=value / key: value forms in text logs.
        regex: new RegExp('\\b(' + escaped + ')\\b\\s*([=:])\\s*([^\\s,;]+)', 'gi'),
        replacement: '$1$2' + DEFAULT_REDACTION
      });
    }
    return out;
  }

  function buildRules(rawRules) {
    var rules = rawRules || {};
    var regexes = [];
    var keyRegexes = buildKeyRegexes(rules.keys || []);

    var patterns = Array.isArray(rules.patterns) ? rules.patterns : [];
    for (var i = 0; i < patterns.length; i += 1) {
      var compiled = compilePattern(patterns[i]);
      if (compiled) regexes.push(compiled);
    }

    return {
      regexes: regexes,
      keyRegexes: keyRegexes,
      redaction: DEFAULT_REDACTION
    };
  }

  function maskText(value, compiledRules) {
    var text = toStringValue(value);
    if (!text) return text;

    var next = text;
    var i;

    for (i = 0; i < compiledRules.regexes.length; i += 1) {
      next = next.replace(compiledRules.regexes[i], compiledRules.redaction);
    }
    for (i = 0; i < compiledRules.keyRegexes.length; i += 1) {
      var item = compiledRules.keyRegexes[i];
      next = next.replace(item.regex, item.replacement);
    }

    return next;
  }

  async function fetchRules(rulesUrl) {
    var endpoint = rulesUrl || '/api/settings/masking/rules';
    var response = await fetch(endpoint, {
      method: 'GET',
      credentials: 'same-origin',
      headers: { 'Accept': 'application/json' }
    });

    if (!response.ok) {
      throw new Error('Failed to fetch masking rules (' + response.status + ')');
    }

    var payload = await response.json();
    if (!payload || payload.ok !== true) {
      throw new Error('Masking rules response was not ok');
    }

    return {
      keys: Array.isArray(payload.keys) ? payload.keys : [],
      patterns: Array.isArray(payload.patterns) ? payload.patterns : []
    };
  }

  function shouldMaskInput(el) {
    if (!el) return false;
    var tag = (el.tagName || '').toLowerCase();
    if (tag === 'textarea') return true;
    if (tag !== 'input') return false;

    var type = (el.getAttribute('type') || 'text').toLowerCase();
    return ['text', 'search', 'email', 'url', 'tel', 'password', 'number', 'hidden'].indexOf(type) !== -1;
  }

  function sanitizeDom(root, compiledRules) {
    var targetRoot = root || document.body;
    var changes = [];

    if (!targetRoot) {
      return {
        restore: function restoreNoop() {},
        changedCount: 0
      };
    }

    var walker = document.createTreeWalker(targetRoot, NodeFilter.SHOW_TEXT, null);
    var node;
    while ((node = walker.nextNode())) {
      var originalText = node.nodeValue;
      var maskedText = maskText(originalText, compiledRules);
      if (maskedText !== originalText) {
        changes.push({ kind: 'text', node: node, original: originalText });
        node.nodeValue = maskedText;
      }
    }

    var elements = targetRoot.querySelectorAll('*');
    for (var i = 0; i < elements.length; i += 1) {
      var el = elements[i];

      if (shouldMaskInput(el) && typeof el.value === 'string') {
        var originalValue = el.value;
        var maskedValue = maskText(originalValue, compiledRules);
        if (maskedValue !== originalValue) {
          changes.push({ kind: 'value', element: el, original: originalValue });
          el.value = maskedValue;
        }
      }

      var attrNames = ['placeholder', 'title', 'aria-label'];
      for (var a = 0; a < attrNames.length; a += 1) {
        var attr = attrNames[a];
        var attrValue = el.getAttribute(attr);
        if (typeof attrValue !== 'string') continue;
        var maskedAttr = maskText(attrValue, compiledRules);
        if (maskedAttr !== attrValue) {
          changes.push({ kind: 'attr', element: el, attr: attr, original: attrValue });
          el.setAttribute(attr, maskedAttr);
        }
      }
    }

    return {
      changedCount: changes.length,
      restore: function restore() {
        for (var j = changes.length - 1; j >= 0; j -= 1) {
          var change = changes[j];
          if (change.kind === 'text' && change.node) {
            change.node.nodeValue = change.original;
          } else if (change.kind === 'value' && change.element) {
            change.element.value = change.original;
          } else if (change.kind === 'attr' && change.element) {
            change.element.setAttribute(change.attr, change.original);
          }
        }
      }
    };
  }

  async function sanitizeDomForScreenshot(options) {
    var opts = options || {};
    var rules = opts.rules || await fetchRules(opts.rulesUrl);
    var compiledRules = buildRules(rules);
    return sanitizeDom(opts.root || document.body, compiledRules);
  }

  global.SOBSDomMasking = {
    fetchRules: fetchRules,
    buildRules: buildRules,
    maskText: maskText,
    sanitizeDomForScreenshot: sanitizeDomForScreenshot
  };
})(window);
