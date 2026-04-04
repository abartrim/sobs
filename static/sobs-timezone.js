(function () {
  'use strict';

  if (window.sobsTimezone) {
    return;
  }

  var GLOBAL_KEY = 'sobs.timezone';
  var RUM_KEY = 'sobs.rum.timezone';
  var browserZone = (Intl.DateTimeFormat().resolvedOptions() || {}).timeZone || 'UTC';
  var activeContext = null;
  var changeListeners = [];
  var zonesPopulated = false;

  function isValidZone(tz) {
    if (!tz) return false;
    try {
      new Intl.DateTimeFormat('en', { timeZone: tz });
      return true;
    } catch (_err) {
      return false;
    }
  }

  function readStoredZone() {
    var saved = localStorage.getItem(GLOBAL_KEY) || localStorage.getItem(RUM_KEY) || '';
    if (saved === 'local') {
      saved = browserZone;
    }
    return isValidZone(saved) ? saved : 'UTC';
  }

  function writeStoredZone(tz) {
    localStorage.setItem(GLOBAL_KEY, tz);
    localStorage.setItem(RUM_KEY, tz);
  }

  function modalEls() {
    return {
      modal: document.getElementById('sobsTzModal'),
      input: document.getElementById('sobs-tz-modal-input'),
      note: document.getElementById('sobs-tz-modal-note'),
      list: document.getElementById('sobs-tz-list'),
      applyBtn: document.getElementById('sobs-tz-apply-btn')
    };
  }

  function populateZoneList() {
    if (zonesPopulated) return;
    var els = modalEls();
    if (!els.list) return;

    var zones;
    try {
      zones = Intl.supportedValuesOf('timeZone');
    } catch (_err) {
      zones = [
        'UTC',
        'America/New_York',
        'America/Chicago',
        'America/Denver',
        'America/Los_Angeles',
        'America/Sao_Paulo',
        'Europe/London',
        'Europe/Paris',
        'Europe/Berlin',
        'Asia/Kolkata',
        'Asia/Tokyo',
        'Asia/Shanghai',
        'Australia/Sydney'
      ];
    }

    var priority = ['UTC'];
    if (browserZone !== 'UTC') priority.push(browserZone);
    var rest = zones.filter(function (z) { return priority.indexOf(z) === -1; });
    var frag = document.createDocumentFragment();

    priority.concat(rest).forEach(function (zone) {
      var opt = document.createElement('option');
      opt.value = zone;
      if (zone === browserZone && zone !== 'UTC') {
        opt.label = zone + ' (browser local)';
      }
      frag.appendChild(opt);
    });

    els.list.appendChild(frag);
    zonesPopulated = true;
  }

  function partsForZone(dateObj, tz) {
    var fmt = new Intl.DateTimeFormat('en-CA', {
      timeZone: tz,
      year: 'numeric', month: '2-digit', day: '2-digit',
      hour: '2-digit', minute: '2-digit', second: '2-digit',
      hour12: false
    });
    var p = { year: '', month: '', day: '', hour: '', minute: '', second: '' };
    fmt.formatToParts(dateObj).forEach(function (part) {
      if (part.type in p) p[part.type] = part.value;
    });
    return p;
  }

  function formatForInput(dateObj, tz) {
    var p = partsForZone(dateObj, tz);
    return p.year + '-' + p.month + '-' + p.day + ' ' + p.hour + ':' + p.minute;
  }

  function formatForDisplay(dateObj, tz, withSeconds) {
    var p = partsForZone(dateObj, tz);
    return p.year + '-' + p.month + '-' + p.day + ' ' + p.hour + ':' + p.minute + (withSeconds ? ':' + p.second : '');
  }

  function parseUtcTimestamp(value) {
    var raw = String(value || '').trim();
    if (!raw) return null;
    if (raw.indexOf('T') === -1) raw = raw.replace(' ', 'T');
    if (!/[zZ]|[+\-]\d\d:?\d\d$/.test(raw)) raw += 'Z';
    var d = new Date(raw);
    return Number.isNaN(d.getTime()) ? null : d;
  }

  function parseYmdHm(value, tz) {
    var m = String(value || '').trim().match(/^(\d{4})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2})(?::(\d{2}))?$/);
    if (!m) return null;

    var y = +m[1];
    var mo = +m[2];
    var d = +m[3];
    var h = +m[4];
    var mi = +m[5];
    var s = +(m[6] || '0');

    if (tz === 'UTC') {
      return new Date(Date.UTC(y, mo - 1, d, h, mi, s));
    }

    var guess = new Date(Date.UTC(y, mo - 1, d, h, mi, s));
    var shown = partsForZone(guess, tz);
    var shownUtc = Date.UTC(+shown.year, +shown.month - 1, +shown.day, +shown.hour, +shown.minute, +shown.second);
    var result = new Date(guess.getTime() - (shownUtc - guess.getTime()));
    return Number.isNaN(result.getTime()) ? null : result;
  }

  function friendlyTzName(tz) {
    if (tz === 'UTC') return 'UTC';
    try {
      var parts = new Intl.DateTimeFormat('en', { timeZone: tz, timeZoneName: 'shortOffset' }).formatToParts(new Date());
      var tzPart = parts.find(function (p) { return p.type === 'timeZoneName'; });
      var offset = tzPart ? tzPart.value.replace('GMT', 'UTC') : '';
      var city = tz.split('/').pop().replace(/_/g, ' ');
      return offset ? offset + ' · ' + city : city;
    } catch (_err) {
      return tz;
    }
  }

  function inferUtcText(text) {
    var raw = String(text || '').trim();
    if (!raw) return '';
    var m = raw.match(/^(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}(?::\d{2}(?:\.\d{1,6})?)?)/);
    return m ? m[1] : '';
  }

  function prepareTimestampElements(ctx) {
    if (!ctx.timestampSelector) {
      ctx.timestampElements = [];
      return;
    }

    var els = Array.from(document.querySelectorAll(ctx.timestampSelector));
    els.forEach(function (el) {
      if (!el.getAttribute('data-utc-ts')) {
        var inferred = inferUtcText(el.textContent || '');
        if (inferred) {
          el.setAttribute('data-utc-ts', inferred);
        }
      }
    });

    ctx.timestampElements = els.filter(function (el) {
      return !!el.getAttribute('data-utc-ts');
    });
  }

  function updateTimestampEl(el, tz) {
    var raw = el.getAttribute('data-utc-ts');
    var d = parseUtcTimestamp(raw);
    if (!d) return;

    var withSeconds = String(el.getAttribute('data-tz-seconds') || '1') !== '0';
    el.textContent = formatForDisplay(d, tz, withSeconds);
    el.setAttribute('title', 'UTC: ' + formatForDisplay(d, 'UTC', true) + ' | ' + tz + ': ' + formatForDisplay(d, tz, true));
  }

  function renderTimestamps(ctx) {
    (ctx.timestampElements || []).forEach(function (el) {
      updateTimestampEl(el, ctx.selectedTz);
    });
  }

  function convertFilterInputs(ctx, fromTz, toTz) {
    [ctx.fromInput, ctx.toInput].forEach(function (inputEl) {
      if (!inputEl) return;
      var value = String(inputEl.value || '').trim();
      if (!value) return;
      var parsed = parseYmdHm(value, fromTz);
      if (!parsed) return;
      inputEl.value = formatForInput(parsed, toTz);
    });
  }

  function applyContextDisplay(ctx) {
    if (ctx.labelEl) {
      ctx.labelEl.textContent = friendlyTzName(ctx.selectedTz);
    }
    if (ctx.badgeBtnEl) {
      ctx.badgeBtnEl.title = 'Display timezone: ' + ctx.selectedTz + ' - click to change';
    }
    if (ctx.noteEl) {
      ctx.noteEl.textContent = 'Times in ' + friendlyTzName(ctx.selectedTz);
    }
    renderTimestamps(ctx);
    changeListeners.forEach(function (cb) {
      cb(ctx.selectedTz);
    });
  }

  function openModal(ctx, evt) {
    if (evt) {
      evt.preventDefault();
      evt.stopPropagation();
    }

    var els = modalEls();
    if (!els.modal || !els.input || !window.bootstrap || !window.bootstrap.Modal) return;

    populateZoneList();
    activeContext = ctx;
    els.input.value = ctx.selectedTz;
    if (els.note) {
      els.note.textContent = '';
      els.note.style.color = '';
    }
    window.bootstrap.Modal.getOrCreateInstance(els.modal).show();
  }

  function bindModalHandlers() {
    var els = modalEls();
    if (!els.modal || !els.input || !els.applyBtn) return;
    if (els.modal.getAttribute('data-sobs-tz-bound') === '1') return;

    els.modal.setAttribute('data-sobs-tz-bound', '1');

    els.input.addEventListener('input', function () {
      if (!els.note) return;
      var value = String(els.input.value || '').trim();
      if (!value) {
        els.note.textContent = '';
        els.note.style.color = '';
        return;
      }
      if (isValidZone(value)) {
        els.note.textContent = 'OK ' + friendlyTzName(value);
        els.note.style.color = 'var(--bs-success)';
      } else {
        els.note.textContent = 'Unknown timezone';
        els.note.style.color = 'var(--bs-danger)';
      }
    });

    els.applyBtn.addEventListener('click', function () {
      if (!activeContext) return;
      var next = String(els.input.value || '').trim();
      if (!isValidZone(next)) return;

      convertFilterInputs(activeContext, activeContext.selectedTz, next);
      activeContext.selectedTz = next;
      writeStoredZone(next);
      applyContextDisplay(activeContext);

      var modalInstance = window.bootstrap.Modal.getInstance(els.modal);
      if (modalInstance) modalInstance.hide();
    });
  }

  function initPage(options) {
    options = options || {};

    var ctx = {
      selectedTz: readStoredZone(),
      fromInput: document.getElementById(options.fromInputId || ''),
      toInput: document.getElementById(options.toInputId || ''),
      formEl: options.formSelector ? document.querySelector(options.formSelector) : null,
      badgeBtnEl: document.getElementById(options.badgeButtonId || ''),
      labelEl: document.getElementById(options.badgeLabelId || ''),
      noteEl: document.getElementById(options.noteId || ''),
      timestampSelector: options.timestampSelector || ''
    };

    prepareTimestampElements(ctx);

    if (ctx.fromInput && ctx.toInput) {
      convertFilterInputs(ctx, 'UTC', ctx.selectedTz);
    }

    applyContextDisplay(ctx);

    if (ctx.formEl && ctx.fromInput && ctx.toInput) {
      ctx.formEl.addEventListener('submit', function () {
        convertFilterInputs(ctx, ctx.selectedTz, 'UTC');
      });
    }

    if (ctx.badgeBtnEl) {
      ctx.badgeBtnEl.addEventListener('click', function (evt) { openModal(ctx, evt); });
      ctx.badgeBtnEl.addEventListener('keydown', function (evt) {
        if (evt.key === 'Enter' || evt.key === ' ') {
          openModal(ctx, evt);
        }
      });
    }

    bindModalHandlers();
    return ctx;
  }

  function refreshTimestamps(selector) {
    var target = selector || '[data-utc-ts]';
    var tz = readStoredZone();
    Array.from(document.querySelectorAll(target)).forEach(function (el) {
      updateTimestampEl(el, tz);
    });
  }

  window.sobsTimezone = {
    initPage: initPage,
    refreshTimestamps: refreshTimestamps,
    zone: readStoredZone,
    formatUtc: function (value, withSeconds) {
      var d = parseUtcTimestamp(value);
      if (!d) return String(value || '');
      return formatForDisplay(d, readStoredZone(), withSeconds !== false);
    },
    fmtHHMM: function (value) {
      var d = parseUtcTimestamp(value);
      if (!d) {
        var s = String(value || '').split(' ');
        return String((s[1] || s[0] || '')).slice(0, 5);
      }
      var p = partsForZone(d, readStoredZone());
      return p.hour + ':' + p.minute;
    },
    onChange: function (cb) {
      if (typeof cb === 'function') {
        changeListeners.push(cb);
      }
    }
  };
})();