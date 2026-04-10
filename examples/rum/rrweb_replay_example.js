/*
 * rrweb-style replay integration example for SOBS RUM.
 *
 * This test-focused variant intentionally includes fields that should be masked
 * by SOBS output redaction (emails, bearer tokens, passwords, API keys, etc.).
 * Use the "simulate-checkout-error" trigger to verify masking in:
 * - RUM timeline/details
 * - Incident + Error views (message/stack/console/replay metadata)
 * - Replay raw JSON modal
 */

(function (global) {
  'use strict';

  function nowTs() {
    return Date.now();
  }

  function buildSensitiveFixture() {
    var ts = String(nowTs());
    return {
      customerEmail: 'customer.' + ts + '@example.com',
      actorEmail: 'ops.' + ts + '@example.com',
      bearerToken: 'Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.' + ts + '.sig',
      apiKey: 'sk_live_' + ts + '_secret',
      password: 'SuperSecret!-' + ts,
      authHeader: 'Authorization: Bearer token-' + ts,
      ssn: '123-45-6789',
      card: '4111111111111111'
    };
  }

  // Placeholder API. Replace with your own upload endpoint.
  async function uploadReplaySnapshot(events, fixture) {
    var replayEnvelope = {
      provider: 'rrweb',
      events: events,
      // Test-only metadata to confirm output masking in replay JSON viewers.
      tags: {
        customer_email: fixture.customerEmail,
        api_key: fixture.apiKey,
        authorization: fixture.bearerToken
      },
      breadcrumbs: [
        {
          level: 'error',
          message: 'Checkout failed for ' + fixture.customerEmail,
          auth: fixture.authHeader,
          password: fixture.password
        }
      ],
      context: {
        account: 'acct-test',
        support_contact: fixture.actorEmail,
        credit_card: fixture.card,
        ssn: fixture.ssn
      }
    };

    var response = await fetch('/api/replay/upload', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(replayEnvelope)
    });
    if (!response.ok) throw new Error('Replay upload failed');
    return response.json(); // expected: { id: 'replay-123', url: 'https://.../replay-123' }
  }

  // Screenshots are binary; we cannot regex-redact pixels server-side like text.
  // This helper shows the pattern: temporarily mask DOM text client-side before
  // capture/upload, then restore original values in a finally block.
  async function createSanitizedScreenshotArtifact(fixture) {
    var screenshotId = 'shot-' + nowTs();
    var domMaskSession = null;

    if (global.SOBSDomMasking && typeof global.SOBSDomMasking.sanitizeDomForScreenshot === 'function') {
      try {
        domMaskSession = await global.SOBSDomMasking.sanitizeDomForScreenshot({
          rulesUrl: '/api/settings/masking/rules'
        });
      } catch (maskErr) {
        console.warn('DOM masking helper failed, continuing without pre-mask', maskErr);
      }
    }

    try {
      // Replace this block with your real pixel redaction pipeline, for example:
      // 1) Capture screenshot to canvas.
      // 2) Blackout known selectors with PII (email, token, payment fields).
      // 3) Blur free-text panels that may include secrets.
      // 4) Upload the sanitized blob only.

      return {
        type: 'screenshot',
        id: screenshotId,
        // Keep URL opaque; avoid PII in path/query params.
        url: 'https://example.com/artifacts/' + screenshotId + '.png',
        provider: 'sanitized-canvas',
        // Optional debug metadata for test visibility (should be masked in UI).
        owner_email: fixture.actorEmail,
        annotation: 'api_key=' + fixture.apiKey
      };
    } finally {
      if (domMaskSession && typeof domMaskSession.restore === 'function') {
        domMaskSession.restore();
      }
    }
  }

  function emitSensitiveConsoleLogs(fixture) {
    console.error('Checkout exception for %s with token %s', fixture.customerEmail, fixture.bearerToken);
    console.warn('password=%s api_key=%s', fixture.password, fixture.apiKey);
    console.info('auth header %s', fixture.authHeader);
  }

  async function attachReplayContextAndCapture(error, fixture) {
    // In a real rrweb integration, this would be recent rrweb event data.
    var replayEvents = [
      {
        type: 'meta',
        ts: nowTs(),
        service: 'checkout-web',
        customer_email: fixture.customerEmail,
        token: fixture.bearerToken
      },
      {
        type: 'custom',
        ts: nowTs(),
        payload: {
          message: 'payment failed for ' + fixture.customerEmail,
          authorization: fixture.authHeader,
          password: fixture.password,
          api_key: fixture.apiKey
        }
      }
    ];

    var replayRef = await uploadReplaySnapshot(replayEvents, fixture);

    // Attach replay metadata to the next captured browser error.
    global.SOBS.setReplayContext(
      {
        id: replayRef.id,
        url: replayRef.url,
        provider: 'rrweb',
        support_contact: fixture.actorEmail
      },
      {
        ttlMs: 15000,
        consumeOnce: true
      }
    );

    // Attach sanitized screenshot metadata only (no raw screenshot bytes here).
    var screenshotRef = await createSanitizedScreenshotArtifact(fixture);
    global.SOBS.setArtifactContext(screenshotRef, { ttlMs: 15000, consumeOnce: true });

    emitSensitiveConsoleLogs(fixture);

    if (error && typeof error === 'object') {
      try {
        error.stack = [
          'Error: checkout failed for ' + fixture.customerEmail,
          '    at submitOrder (checkout.js:52:19)',
          '    at postPayment (payment.js:88:7) // ' + fixture.authHeader,
          '    at fetchToken (auth.js:14:5) // api_key=' + fixture.apiKey
        ].join('\n');
      } catch (_err) {
        // Ignore if stack is read-only in this runtime.
      }
    }

    global.SOBS.captureException(error, {
      errorSource: 'captureException',
      message:
        'Checkout failed for ' + fixture.customerEmail +
        ' auth=' + fixture.authHeader +
        ' password=' + fixture.password +
        ' card=' + fixture.card +
        ' ssn=' + fixture.ssn
    });
  }

  // Demo trigger.
  global.addEventListener('click', async function onDemoClick(evt) {
    var target = evt.target;
    if (!target || target.id !== 'simulate-checkout-error') return;

    var fixture = buildSensitiveFixture();

    try {
      throw new Error('Checkout confirmation failed for ' + fixture.customerEmail);
    } catch (error) {
      try {
        await attachReplayContextAndCapture(error, fixture);
      } catch (uploadErr) {
        // Fall back to normal error capture when replay upload is unavailable.
        global.SOBS.captureException(uploadErr, {
          errorSource: 'replay-upload',
          message: 'Replay upload failed for ' + fixture.customerEmail + ' api_key=' + fixture.apiKey
        });
      }
    }
  });
})(window);
