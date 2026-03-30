/**
 * SOBS Date Range Picker
 * A lightweight Bootstrap-compatible date/time range picker with quick presets.
 * Usage: call sobsDateRangePicker.init() after DOM is loaded, or attach via
 * data-drp-from / data-drp-to attributes on the toggle button.
 */
(function () {
  'use strict';

  const QUICK_PRESETS = [
    { label: '5m',  minutes: 5 },
    { label: '15m', minutes: 15 },
    { label: '30m', minutes: 30 },
    { label: '1h',  minutes: 60 },
    { label: '2h',  minutes: 120 },
    { label: '6h',  minutes: 360 },
    { label: '12h', minutes: 720 },
    { label: '24h', minutes: 1440 },
    { label: '2d',  minutes: 2880 },
    { label: '7d',  minutes: 10080 },
    { label: '14d', minutes: 20160 },
    { label: '30d', minutes: 43200 },
    { label: '3mo', minutes: 129600 },
    { label: '6mo', minutes: 259200 },
    { label: '1y',  minutes: 525600 },
  ];

  function pad2(n) {
    return String(n).padStart(2, '0');
  }

  /**
   * Format a Date to a compact local timestamp string: YYYY-MM-DD HH:MM
   */
  function toHuman(d) {
    return d.getFullYear() + '-' +
      pad2(d.getMonth() + 1) + '-' +
      pad2(d.getDate()) + ' ' +
      pad2(d.getHours()) + ':' +
      pad2(d.getMinutes());
  }

  /**
   * Parse an ISO / datetime-local string into a Date, or return null.
   */
  function parseDate(s) {
    if (!s) return null;
    var raw = String(s).trim();
    if (!raw) return null;
    var m = raw.match(/^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2})(?::(\d{2})(?:\.(\d{1,6}))?)?(?:\s?(Z|[+-]\d{2}:?\d{2}))?$/);
    if (m) {
      var tz = m[8] || '';
      if (tz) {
        var normalizedTz = tz;
        if (tz !== 'Z' && tz.length === 5) {
          normalizedTz = tz.slice(0, 3) + ':' + tz.slice(3);
        }
        var secText = m[6] ? m[6] : '00';
        var iso = m[1] + '-' + m[2] + '-' + m[3] + 'T' + m[4] + ':' + m[5] + ':' + secText + normalizedTz;
        var dTz = new Date(iso);
        return isNaN(dTz.getTime()) ? null : dTz;
      }
      var sec = m[6] ? parseInt(m[6], 10) : 0;
      var dLocal = new Date(
        parseInt(m[1], 10),
        parseInt(m[2], 10) - 1,
        parseInt(m[3], 10),
        parseInt(m[4], 10),
        parseInt(m[5], 10),
        sec
      );
      return isNaN(dLocal.getTime()) ? null : dLocal;
    }
    var d = new Date(raw.replace(' ', 'T'));
    return isNaN(d.getTime()) ? null : d;
  }

  function normalizeInputDisplay(inputEl) {
    if (!inputEl || !inputEl.value) return;
    var d = parseDate(inputEl.value);
    if (!d) return;
    inputEl.value = toHuman(d);
  }

  /**
   * Convert a Date to the value string expected by <input type="datetime-local">
   * (local-time, format: YYYY-MM-DDTHH:MM)
   */
  function toDatetimeLocal(d) {
    if (!d) return '';
    return d.getFullYear() + '-' +
      pad2(d.getMonth() + 1) + '-' +
      pad2(d.getDate()) + 'T' +
      pad2(d.getHours()) + ':' +
      pad2(d.getMinutes());
  }

  /**
   * Build the dropdown HTML string for a picker instance.
   */
  function buildDropdownHtml(uid) {
    var rows = '';
    QUICK_PRESETS.forEach(function (p) {
      rows += '<button type="button" class="btn btn-outline-secondary btn-sm drp-preset" ' +
        'data-minutes="' + p.minutes + '">' + p.label + '</button>';
    });

    return '<div id="drp-menu-' + uid + '" class="drp-dropdown-menu card border-secondary shadow" ' +
      'style="display:none;position:absolute;z-index:1070;min-width:320px;">' +
      '<div class="card-body p-3">' +
      '<p class="mb-2 text-secondary small fw-semibold"><i class="bi bi-lightning-charge me-1"></i>Quick ranges</p>' +
      '<div class="d-flex flex-wrap gap-1 mb-3">' + rows + '</div>' +
      '<hr class="my-2 border-secondary">' +
      '<p class="mb-2 text-secondary small fw-semibold"><i class="bi bi-calendar3 me-1"></i>Custom range</p>' +
      '<div class="mb-2">' +
      '<label class="form-label small text-secondary mb-1">From</label>' +
      '<input type="datetime-local" id="drp-from-custom-' + uid + '" class="form-control form-control-sm drp-custom-from">' +
      '</div>' +
      '<div class="mb-3">' +
      '<label class="form-label small text-secondary mb-1">To</label>' +
      '<input type="datetime-local" id="drp-to-custom-' + uid + '" class="form-control form-control-sm drp-custom-to">' +
      '</div>' +
      '<div class="d-flex gap-2">' +
      '<button type="button" class="btn btn-primary btn-sm flex-grow-1 drp-apply">Apply</button>' +
      '<button type="button" class="btn btn-outline-secondary btn-sm drp-clear">Clear</button>' +
      '</div>' +
      '</div>' +
      '</div>';
  }

  var _uid = 0;

  /**
   * Attach a date range picker to a toggle button element.
   * The toggle button must have data-drp-from and data-drp-to attributes
   * containing the IDs of the from/to text inputs it controls.
   */
  function attach(toggleBtn) {
    var fromInputId = toggleBtn.getAttribute('data-drp-from');
    var toInputId = toggleBtn.getAttribute('data-drp-to');
    var formEl = toggleBtn.closest('form');

    if (!fromInputId || !toInputId || !formEl) return;

    var fromInput = document.getElementById(fromInputId);
    var toInput = document.getElementById(toInputId);
    if (!fromInput || !toInput) return;

    // Normalize any server-rendered timestamp strings for consistent display.
    normalizeInputDisplay(fromInput);
    normalizeInputDisplay(toInput);

    _uid += 1;
    var uid = _uid;

    // Build and insert dropdown HTML at body level so input-group sizing stays compact.
    document.body.insertAdjacentHTML('beforeend', buildDropdownHtml(uid));
    var menu = document.getElementById('drp-menu-' + uid);

    // Mark as initialised
    toggleBtn.setAttribute('data-drp-uid', uid);

    function positionMenu() {
      var rect = toggleBtn.getBoundingClientRect();
      var menuWidth = menu.offsetWidth || 320;
      var left = rect.right + window.scrollX - menuWidth;
      var minLeft = window.scrollX + 8;
      if (left < minLeft) left = minLeft;
      menu.style.top = (rect.bottom + window.scrollY + 4) + 'px';
      menu.style.left = left + 'px';
    }

    // --- Toggle open/close ---
    toggleBtn.addEventListener('click', function (e) {
      e.stopPropagation();
      var isOpen = menu.style.display !== 'none';
      // Close any other open pickers
      document.querySelectorAll('.drp-dropdown-menu').forEach(function (m) {
        m.style.display = 'none';
      });
      if (!isOpen) {
        // Pre-fill custom inputs from current text values
        var fromCustom = menu.querySelector('.drp-custom-from');
        var toCustom = menu.querySelector('.drp-custom-to');
        var dFrom = parseDate(fromInput.value);
        var dTo = parseDate(toInput.value);
        fromCustom.value = dFrom ? toDatetimeLocal(dFrom) : '';
        toCustom.value = dTo ? toDatetimeLocal(dTo) : '';
        menu.style.display = 'block';
        positionMenu();
      }
    });

    // --- Quick presets ---
    menu.querySelectorAll('.drp-preset').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var minutes = parseInt(btn.getAttribute('data-minutes'), 10);
        var now = new Date();
        var from = new Date(now.getTime() - minutes * 60 * 1000);
        fromInput.value = toHuman(from);
        toInput.value = '';
        menu.style.display = 'none';
        formEl.submit();
      });
    });

    // --- Apply custom range ---
    menu.querySelector('.drp-apply').addEventListener('click', function () {
      var fromCustom = menu.querySelector('.drp-custom-from');
      var toCustom = menu.querySelector('.drp-custom-to');
      var dFrom = parseDate(fromCustom.value);
      var dTo = parseDate(toCustom.value);
      fromInput.value = dFrom ? toHuman(dFrom) : '';
      toInput.value = dTo ? toHuman(dTo) : '';
      menu.style.display = 'none';
      formEl.submit();
    });

    // --- Clear ---
    menu.querySelector('.drp-clear').addEventListener('click', function () {
      fromInput.value = '';
      toInput.value = '';
      menu.style.display = 'none';
      formEl.submit();
    });

    // Close when clicking outside
    document.addEventListener('click', function (e) {
      if (!menu.contains(e.target) && !toggleBtn.contains(e.target)) {
        menu.style.display = 'none';
      }
    });

    window.addEventListener('resize', function () {
      if (menu.style.display !== 'none') positionMenu();
    });

    window.addEventListener('scroll', function () {
      if (menu.style.display !== 'none') positionMenu();
    }, true);
  }

  /**
   * Initialise all toggle buttons on the page.
   */
  function init() {
    document.querySelectorAll('[data-drp-toggle]').forEach(function (btn) {
      if (!btn.getAttribute('data-drp-uid')) {
        attach(btn);
      }
    });
  }

  // Auto-init on DOMContentLoaded
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }

  window.sobsDateRangePicker = { init: init, attach: attach };
})();
