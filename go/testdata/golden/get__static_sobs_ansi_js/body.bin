(function () {
  'use strict';

  if (window.sobsAnsi) {
    return;
  }

  var STORAGE_KEY = 'sobs.ansi.colors';

  // Standard foreground color codes: [light-mode color, dark-mode color]
  var FG_COLORS = {
    30: ['#1a1a1a', '#9da5b4'],   // Black
    31: ['#c0392b', '#e06c75'],   // Red
    32: ['#1e8449', '#98c379'],   // Green
    33: ['#7d6608', '#e5c07b'],   // Yellow
    34: ['#1a5276', '#61afef'],   // Blue
    35: ['#6c3483', '#c678dd'],   // Magenta
    36: ['#0e6655', '#56b6c2'],   // Cyan
    37: ['#444444', '#abb2bf'],   // White (light gray)
    90: ['#666666', '#5c6370'],   // Bright Black (dark gray)
    91: ['#c0392b', '#be5046'],   // Bright Red
    92: ['#1e8449', '#98c379'],   // Bright Green
    93: ['#b7950b', '#e5c07b'],   // Bright Yellow
    94: ['#2980b9', '#61afef'],   // Bright Blue
    95: ['#8e44ad', '#c678dd'],   // Bright Magenta
    96: ['#148f77', '#56b6c2'],   // Bright Cyan
    97: ['#222222', '#ffffff']    // Bright White
  };

  // Standard background color codes: [light-mode color, dark-mode color]
  var BG_COLORS = {
    40:  ['#d5d8dc', '#21252b'],  // Black bg
    41:  ['#fadbd8', '#3b1a1a'],  // Red bg
    42:  ['#d5f5e3', '#1a3020'],  // Green bg
    43:  ['#fef9e7', '#332b10'],  // Yellow bg
    44:  ['#d6eaf8', '#1a2535'],  // Blue bg
    45:  ['#e8daef', '#271a35'],  // Magenta bg
    46:  ['#d1f2eb', '#1a302e'],  // Cyan bg
    47:  ['#f0f0f0', '#3e4451'],  // White bg
    100: ['#aab7b8', '#3e4451'],  // Bright Black bg
    101: ['#e74c3c', '#be5046'],  // Bright Red bg
    102: ['#27ae60', '#4a7c59'],  // Bright Green bg
    103: ['#f39c12', '#665f1f'],  // Bright Yellow bg
    104: ['#2980b9', '#2a4f6a'],  // Bright Blue bg
    105: ['#8e44ad', '#5c2d8c'],  // Bright Magenta bg
    106: ['#16a085', '#1e5c56'],  // Bright Cyan bg
    107: ['#ffffff', '#abb2bf']   // Bright White bg
  };

  function isDarkMode() {
    return document.documentElement.getAttribute('data-bs-theme') === 'dark' ||
      (window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
  }

  // Convert a 256-color cube index (0-5) to an sRGB channel value.
  function cube6(i) { return i === 0 ? 0 : 55 + i * 40; }

  // Convert a 256-color palette index to an 'rgb(r,g,b)' string.
  // Returns null for indices 0-15 so that theme-aware FG/BG_COLORS tables are used instead.
  function ansi256ToRgb(idx) {
    if (idx < 16) { return null; }
    if (idx < 232) {
      var n = idx - 16;
      return 'rgb(' + cube6(Math.floor(n / 36)) + ',' + cube6(Math.floor((n % 36) / 6)) + ',' + cube6(n % 6) + ')';
    }
    var gray = 8 + (idx - 232) * 10;
    return 'rgb(' + gray + ',' + gray + ',' + gray + ')';
  }

  // Parse ANSI SGR escape sequences into an array of styled text segments.
  // Each segment: { text, fg, bg, fgRgb, bgRgb, bold, dim, italic, underline }
  function parseAnsi(text) {
    var segments = [];
    var state = { fg: null, bg: null, fgRgb: null, bgRgb: null, bold: false, dim: false, italic: false, underline: false };
    var buf = '';
    var i = 0;
    var len = text.length;

    while (i < len) {
      if (text.charCodeAt(i) === 0x1b && i + 1 < len && text[i + 1] === '[') {
        // Flush buffered plain text
        if (buf) {
          segments.push({ text: buf, fg: state.fg, bg: state.bg, fgRgb: state.fgRgb, bgRgb: state.bgRgb, bold: state.bold, dim: state.dim, italic: state.italic, underline: state.underline });
          buf = '';
        }
        // Scan for the final byte (letter in @-~)
        var start = i + 2;
        var end = start;
        while (end < len && (text.charCodeAt(end) < 0x40 || text.charCodeAt(end) > 0x7e)) {
          end++;
        }
        if (end >= len) {
          // Incomplete sequence — stop
          break;
        }
        var finalByte = text[end];
        var paramStr = text.slice(start, end);
        i = end + 1;

        if (finalByte !== 'm') {
          // Non-SGR sequence (cursor moves, erase, etc.) — ignore
          continue;
        }

        // Parse SGR parameters
        var codes = paramStr === '' ? [0] : paramStr.split(';').map(Number);
        var j = 0;
        while (j < codes.length) {
          var code = codes[j];
          if (code === 0) {
            state.fg = null; state.bg = null; state.fgRgb = null; state.bgRgb = null;
            state.bold = false; state.dim = false; state.italic = false; state.underline = false;
          } else if (code === 1) {
            state.bold = true;
          } else if (code === 2) {
            state.dim = true;
          } else if (code === 3) {
            state.italic = true;
          } else if (code === 4) {
            state.underline = true;
          } else if (code === 22) {
            state.bold = false; state.dim = false;
          } else if (code === 23) {
            state.italic = false;
          } else if (code === 24) {
            state.underline = false;
          } else if ((code >= 30 && code <= 37) || (code >= 90 && code <= 97)) {
            state.fg = code; state.fgRgb = null;
          } else if (code === 38 && j + 1 < codes.length) {
            if (codes[j + 1] === 5 && j + 2 < codes.length) {
              // 256-color fg: 38;5;n
              var idx = codes[j + 2];
              var rgb256 = ansi256ToRgb(idx);
              if (rgb256) {
                state.fgRgb = rgb256; state.fg = null;
              } else if (idx < 8) {
                state.fg = 30 + idx; state.fgRgb = null;
              } else {
                state.fg = 90 + (idx - 8); state.fgRgb = null;
              }
              j += 2;
            } else if (codes[j + 1] === 2 && j + 4 < codes.length) {
              // True-color fg: 38;2;r;g;b
              state.fgRgb = 'rgb(' + codes[j + 2] + ',' + codes[j + 3] + ',' + codes[j + 4] + ')';
              state.fg = null;
              j += 4;
            }
          } else if (code === 39) {
            state.fg = null; state.fgRgb = null;
          } else if ((code >= 40 && code <= 47) || (code >= 100 && code <= 107)) {
            state.bg = code; state.bgRgb = null;
          } else if (code === 48 && j + 1 < codes.length) {
            if (codes[j + 1] === 5 && j + 2 < codes.length) {
              // 256-color bg: 48;5;n
              idx = codes[j + 2];
              var bgRgb256 = ansi256ToRgb(idx);
              if (bgRgb256) {
                state.bgRgb = bgRgb256; state.bg = null;
              } else if (idx < 8) {
                state.bg = 40 + idx; state.bgRgb = null;
              } else {
                state.bg = 100 + (idx - 8); state.bgRgb = null;
              }
              j += 2;
            } else if (codes[j + 1] === 2 && j + 4 < codes.length) {
              state.bgRgb = 'rgb(' + codes[j + 2] + ',' + codes[j + 3] + ',' + codes[j + 4] + ')';
              state.bg = null;
              j += 4;
            }
          } else if (code === 49) {
            state.bg = null; state.bgRgb = null;
          }
          j++;
        }
      } else {
        buf += text[i];
        i++;
      }
    }

    if (buf) {
      segments.push({ text: buf, fg: state.fg, bg: state.bg, fgRgb: state.fgRgb, bgRgb: state.bgRgb, bold: state.bold, dim: state.dim, italic: state.italic, underline: state.underline });
    }

    return segments;
  }

  function escapeHtml(s) {
    return s
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }

  function renderAnsi(text, dark) {
    var segments = parseAnsi(text);
    var html = '';
    var themeIdx = dark ? 1 : 0;

    for (var i = 0; i < segments.length; i++) {
      var seg = segments[i];
      var styles = [];

      var fgColor = seg.fgRgb;
      if (!fgColor && seg.fg !== null && FG_COLORS[seg.fg]) {
        fgColor = FG_COLORS[seg.fg][themeIdx];
      }
      if (fgColor) { styles.push('color:' + fgColor); }

      var bgColor = seg.bgRgb;
      if (!bgColor && seg.bg !== null && BG_COLORS[seg.bg]) {
        bgColor = BG_COLORS[seg.bg][themeIdx];
      }
      if (bgColor) { styles.push('background-color:' + bgColor); }

      if (seg.bold) { styles.push('font-weight:bold'); }
      if (seg.dim) { styles.push('opacity:0.6'); }
      if (seg.italic) { styles.push('font-style:italic'); }
      if (seg.underline) { styles.push('text-decoration:underline'); }

      var escaped = escapeHtml(seg.text);
      if (styles.length) {
        html += '<span style="' + styles.join(';') + '">' + escaped + '</span>';
      } else {
        html += escaped;
      }
    }

    return html;
  }

  function stripAnsi(text) {
    // Strip all CSI (ESC [) sequences
    return text.replace(/\x1b\[[0-9;]*[a-zA-Z]/g, '');
  }

  // Apply or remove ANSI coloring on a single body-text element
  function applyToElement(el, enabled, dark) {
    var raw = el.getAttribute('data-ansi-raw');
    if (raw === null) {
      // First time: capture original text (with ANSI codes)
      raw = el.textContent;
      el.setAttribute('data-ansi-raw', raw);
    }
    if (enabled) {
      el.innerHTML = renderAnsi(raw, dark);
    } else {
      el.textContent = stripAnsi(raw);
    }
  }

  // Apply to all body-text spans inside a container
  function applyToContainer(containerEl, enabled) {
    if (!containerEl) { return; }
    var dark = isDarkMode();
    var els = containerEl.querySelectorAll('.log-body-text');
    for (var i = 0; i < els.length; i++) {
      applyToElement(els[i], enabled, dark);
    }
  }

  // ---- Public API ----

  window.sobsAnsi = {

    initPage: function (options) {
      options = options || {};
      var containerEl = document.getElementById(options.containerId || 'logsTableContainer');
      var btnEl = document.getElementById(options.buttonId || 'logs-ansi-btn');

      // Read stored preference (default: off)
      var enabled = localStorage.getItem(STORAGE_KEY) === '1';

      function updateButton() {
        if (!btnEl) { return; }
        if (enabled) {
          btnEl.classList.add('sobs-ansi-active');
          btnEl.setAttribute('title', 'Colored log output: on — click to disable');
          btnEl.setAttribute('aria-pressed', 'true');
        } else {
          btnEl.classList.remove('sobs-ansi-active');
          btnEl.setAttribute('title', 'Colored log output: off — click to enable');
          btnEl.setAttribute('aria-pressed', 'false');
        }
      }

      function apply() {
        applyToContainer(containerEl, enabled);
        updateButton();
      }

      apply();

      if (btnEl) {
        btnEl.addEventListener('click', function (evt) {
          evt.preventDefault();
          evt.stopPropagation();
          enabled = !enabled;
          localStorage.setItem(STORAGE_KEY, enabled ? '1' : '0');
          apply();
        });
        btnEl.addEventListener('keydown', function (evt) {
          if (evt.key === 'Enter' || evt.key === ' ') {
            evt.preventDefault();
            evt.stopPropagation();
            enabled = !enabled;
            localStorage.setItem(STORAGE_KEY, enabled ? '1' : '0');
            apply();
          }
        });
      }

      // Return a context object for use by live-mode row injection
      return {
        isEnabled: function () { return enabled; },
        applyToElement: function (el) {
          applyToElement(el, enabled, isDarkMode());
        }
      };
    }
  };

})();
