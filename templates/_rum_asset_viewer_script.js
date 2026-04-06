// Shared RUM asset/replay modal logic for Errors and Traces pages.
(function () {
  const modalEl = document.getElementById('rumAssetViewerModal');
  if (!modalEl) return;

  const hasBootstrapModal = !!(window.bootstrap && window.bootstrap.Modal);
  const modal = hasBootstrapModal
    ? window.bootstrap.Modal.getOrCreateInstance(modalEl)
    : {
        show() {
          modalEl.style.display = 'block';
          modalEl.classList.add('show');
          modalEl.removeAttribute('aria-hidden');
          document.body.classList.add('modal-open');
        },
        hide() {
          modalEl.classList.remove('show');
          modalEl.style.display = 'none';
          modalEl.setAttribute('aria-hidden', 'true');
          document.body.classList.remove('modal-open');
        }
      };

  const titleEl = document.getElementById('rumAssetViewerLabel');
  const rawEl = document.getElementById('rumAssetViewerOpenRaw');
  const statusEl = document.getElementById('rumAssetViewerStatus');
  const errorEl = document.getElementById('rumAssetViewerError');

  const imgWrap = document.getElementById('rumAssetViewerImageWrap');
  const imgEl = document.getElementById('rumAssetViewerImage');
  const frameWrap = document.getElementById('rumAssetViewerFrameWrap');
  const frameEl = document.getElementById('rumAssetViewerFrame');
  const replayWrap = document.getElementById('rumAssetViewerReplayWrap');
  const replayPlayer = document.getElementById('rumAssetViewerReplayPlayer');
  const replayRaw = document.getElementById('rumAssetViewerReplayRaw');
  const replayJson = document.getElementById('rumAssetViewerReplayJson');
  let replayRawText = '';
  let replayRawParsed = null;

  if (!hasBootstrapModal) {
    modalEl.querySelectorAll('[data-bs-dismiss="modal"], .btn-close').forEach((btn) => {
      btn.addEventListener('click', () => modal.hide());
    });
    modalEl.addEventListener('click', function (evt) {
      if (evt.target === modalEl) modal.hide();
    });
  }

  let rrwebPlayerLoadPromise = null;

  function hideAll() {
    imgWrap.classList.add('d-none');
    frameWrap.classList.add('d-none');
    replayWrap.classList.add('d-none');
    replayPlayer.classList.add('d-none');
    errorEl.classList.add('d-none');
    errorEl.textContent = '';
    statusEl.textContent = '';
    imgEl.removeAttribute('src');
    frameEl.removeAttribute('src');
    replayPlayer.innerHTML = '';
    if (replayRaw) replayRaw.open = false;
    replayRawText = '';
    replayRawParsed = null;
    replayJson.textContent = '';
  }

  function setError(message) {
    errorEl.textContent = String(message || 'Unable to load asset');
    errorEl.classList.remove('d-none');
  }

  function looksLikeImage(url) {
    return /\.(png|jpe?g|webp|gif)(\?|$)/i.test(String(url || ''));
  }

  function setReplayModalOpenState(isOpen) {
    document.documentElement.classList.toggle('rum-replay-modal-open', !!isOpen);
    document.body.classList.toggle('rum-replay-modal-open', !!isOpen);
  }

  function toEpochMs(value) {
    var n = Number(value);
    if (!Number.isNaN(n)) return n;
    var d = new Date(String(value || ''));
    return Number.isNaN(d.getTime()) ? null : d.getTime();
  }

  // Keep timeline rendering optional so replay still works if timeline markup is absent.
  function renderReplayErrorTimeline() {}

  function loadRrwebPlayer() {
    if (window.rrwebPlayer) return Promise.resolve(true);
    if (rrwebPlayerLoadPromise) return rrwebPlayerLoadPromise;

    rrwebPlayerLoadPromise = new Promise((resolve, reject) => {
      const cssId = 'rum-rrweb-player-css';
      if (!document.getElementById(cssId)) {
        const link = document.createElement('link');
        link.id = cssId;
        link.rel = 'stylesheet';
        link.href = 'https://cdn.jsdelivr.net/npm/rrweb-player@latest/dist/style.css';
        document.head.appendChild(link);
      }

      const script = document.createElement('script');
      script.src = 'https://cdn.jsdelivr.net/npm/rrweb-player@latest/dist/index.js';
      script.async = true;
      script.onload = () => resolve(!!window.rrwebPlayer);
      script.onerror = () => reject(new Error('Failed to load rrweb-player'));
      document.head.appendChild(script);
    });

    return rrwebPlayerLoadPromise;
  }

  async function renderReplay(url) {
    replayWrap.classList.remove('d-none');
    statusEl.textContent = 'Loading replay payload...';

    const resp = await fetch(url, { credentials: 'same-origin' });
    if (!resp.ok) throw new Error('Replay fetch failed with HTTP ' + resp.status);

    const text = await resp.text();
    replayRawText = text;
    replayJson.textContent = '';

    let parsed = null;
    try {
      parsed = JSON.parse(text);
    } catch (_) {
      replayRawParsed = null;
      statusEl.textContent = 'Replay payload is not JSON. Showing raw content.';
      return;
    }
    replayRawParsed = parsed;

    if (!parsed || !Array.isArray(parsed.events) || !parsed.events.length) {
      statusEl.textContent = 'Replay JSON loaded (no events array found).';
      return;
    }

    const startTs = toEpochMs(parsed.events[0] && parsed.events[0].timestamp);
    const endTs = toEpochMs(parsed.events[parsed.events.length - 1] && parsed.events[parsed.events.length - 1].timestamp);
    renderReplayErrorTimeline(parsed, startTs, endTs);

    statusEl.textContent = 'Replay JSON loaded (' + parsed.events.length + ' events).';
    try {
      await loadRrwebPlayer();
      if (!window.rrwebPlayer) return;
      replayPlayer.classList.remove('d-none');
      const playerWidth = Math.max(480, Math.floor(replayPlayer.clientWidth || replayWrap.clientWidth || 1100));
      const playerHeight = Math.max(320, Math.floor(window.innerHeight * 0.5));
      new window.rrwebPlayer({
        target: replayPlayer,
        props: {
          events: parsed.events,
          width: playerWidth,
          height: playerHeight,
          autoPlay: false,
          showController: true
        }
      });
      // If controller is not rendered by rrweb-player, surface that in status text.
      setTimeout(function () {
        var hasController = !!replayPlayer.querySelector('.rr-controller, .rr-player__controller, .controller');
        if (!hasController) {
          statusEl.textContent = 'Replay loaded, but rrweb controls were not rendered by this payload/player version.';
        }
      }, 0);
      statusEl.textContent = 'Replay ready (' + parsed.events.length + ' events).';
    } catch (err) {
      statusEl.textContent = 'Replay JSON loaded; rrweb-player unavailable. Showing raw content.';
    }
  }

  if (replayRaw) {
    replayRaw.addEventListener('toggle', function () {
      if (!replayRaw.open) return;
      if (replayJson.textContent) return;
      if (replayRawParsed) {
        replayJson.textContent = JSON.stringify(replayRawParsed, null, 2);
        return;
      }
      replayJson.textContent = replayRawText || '';
    });
  }

  async function openViewer(kind, url, label) {
    hideAll();
    titleEl.textContent = label || 'RUM Asset Viewer';
    rawEl.href = url;
    setReplayModalOpenState(true);
    modal.show();

    try {
      if (kind === 'artifact') {
        if (looksLikeImage(url)) {
          imgWrap.classList.remove('d-none');
          imgEl.src = url;
          statusEl.textContent = 'Image preview loaded.';
        } else {
          frameWrap.classList.remove('d-none');
          frameEl.src = url;
          statusEl.textContent = 'Rendering asset in embedded frame.';
        }
        return;
      }

      await renderReplay(url);
    } catch (err) {
      setError(err && err.message ? err.message : String(err));
    }
  }

  document.addEventListener('click', function (evt) {
    const btn = evt.target.closest('.js-rum-viewer-open');
    if (!btn) return;
    evt.preventDefault();
    openViewer(
      btn.getAttribute('data-rum-view-kind') || 'artifact',
      btn.getAttribute('data-rum-view-url') || '',
      btn.getAttribute('data-rum-view-label') || 'RUM Asset Viewer'
    );
  });

  modalEl.addEventListener('hidden.bs.modal', function () {
    setReplayModalOpenState(false);
    hideAll();
  });

  modalEl.addEventListener('hide.bs.modal', function () {
    setReplayModalOpenState(false);
  });

  if (!hasBootstrapModal) {
    const originalHide = modal.hide.bind(modal);
    modal.hide = function () {
      setReplayModalOpenState(false);
      originalHide();
      hideAll();
    };
  }
})();
