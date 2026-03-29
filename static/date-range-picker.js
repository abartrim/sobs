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

  /**
   * Format a Date to ISO 8601 UTC string truncated to seconds: YYYY-MM-DDTHH:MM:SSZ
   */
  function toIso(d) {
    return d.toISOString().replace(/\.\d{3}Z$/, 'Z');
  }

  /**
   * Parse an ISO / datetime-local string into a Date, or return null.
   */
  function parseDate(s) {
    if (!s) return null;
    var d = new Date(s);
    return isNaN(d.getTime()) ? null : d;
  }

  /**
   * Convert a Date to the value string expected by <input type="datetime-local">
   * (local-time, format: YYYY-MM-DDTHH:MM)
   */
  function toDatetimeLocal(d) {
    if (!d) return '';
    var pad = function (n) { return String(n).padStart(2, '0'); };
    return d.getFullYear() + '-' +
      pad(d.getMonth() + 1) + '-' +
      pad(d.getDate()) + 'T' +
      pad(d.getHours()) + ':' +
      pad(d.getMinutes());
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
      'style="display:none;position:absolute;z-index:1070;min-width:320px;top:100%;right:0;">' +
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

    _uid += 1;
    var uid = _uid;

    // Wrap toggle button in a relative container for dropdown positioning
    var wrapper = document.createElement('div');
    wrapper.className = 'drp-wrapper position-relative d-inline-block';
    toggleBtn.parentNode.insertBefore(wrapper, toggleBtn);
    wrapper.appendChild(toggleBtn);

    // Build and insert dropdown HTML
    wrapper.insertAdjacentHTML('beforeend', buildDropdownHtml(uid));
    var menu = wrapper.querySelector('#drp-menu-' + uid);

    // Mark as initialised
    toggleBtn.setAttribute('data-drp-uid', uid);

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
      }
    });

    // --- Quick presets ---
    menu.querySelectorAll('.drp-preset').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var minutes = parseInt(btn.getAttribute('data-minutes'), 10);
        var now = new Date();
        var from = new Date(now.getTime() - minutes * 60 * 1000);
        fromInput.value = toIso(from);
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
      fromInput.value = dFrom ? toIso(dFrom) : '';
      toInput.value = dTo ? toIso(dTo) : '';
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
      if (!wrapper.contains(e.target)) {
        menu.style.display = 'none';
      }
    });
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
