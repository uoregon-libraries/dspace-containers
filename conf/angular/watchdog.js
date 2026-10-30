// Watchdog for the DSpace UI's SSR workers, run by pm2 alongside them (see
// dspace-ui.config.js).
//
// When REST goes away, the SSR workers can get stuck: they keep failing every
// server-rendered page with a 500 ("No _links section found at .../server/api")
// long after REST is back, without ever calling REST again. The processes stay
// up, so the container's restart policy never kicks in. This probes a
// server-rendered page every INTERVAL_MS, and if it fails
// FAILURES_BEFORE_RESTART times in a row while REST itself answers, stops pm2
// so the container exits and comes back with fresh workers.
//
// This would be a HEALTHCHECK, but podman builds OCI images, which drop
// HEALTHCHECK, and podman-compose can't set --health-on-failure anyway.

'use strict';

const http = require('node:http');

const INTERVAL_MS = 30_000;
const TIMEOUT_MS = 15_000;
const FAILURES_BEFORE_RESTART = 4;

// SSR only renders requests whose Host matches the public UI URL. Anything
// else gets the client-side shell, which loads fine even when SSR is broken.
const uiURL = 'http://127.0.0.1:4000/home';
const uiHeaders = { Host: new URL(process.env.DSPACE_UI_BASEURL).host };
const restURL = `${process.env.DSPACE_REST_SSRBASEURL}/api`;

let failures = 0;

// Resolves to null if url answers with a non-5xx status, otherwise to why it
// didn't. A 4xx is a config problem a restart can't fix, so it counts as up.
function probe(url, headers = {}) {
  return new Promise((resolve) => {
    // A new connection per probe (no keep-alive agent) so pm2 hands each one
    // to the next SSR worker instead of always reusing the same one
    const req = http.get(url, { headers, agent: false, timeout: TIMEOUT_MS }, (res) => {
      res.resume();
      resolve(res.statusCode < 500 ? null : `HTTP ${res.statusCode}`);
    });
    req.on('timeout', () => req.destroy(new Error(`no response in ${TIMEOUT_MS / 1000}s`)));
    req.on('error', (err) => resolve(err.message));
  });
}

async function check() {
  const uiErr = await probe(uiURL, uiHeaders);
  if (!uiErr) {
    if (failures > 0) {
      console.log(`[watchdog] SSR recovered after ${failures} failed check(s)`);
    }
    failures = 0;
    return;
  }

  // A REST outage breaks SSR too, but restarting the UI can't fix that, so
  // only count failures while REST is answering
  const restErr = await probe(restURL);
  if (restErr) {
    console.log(`[watchdog] SSR check failed (${uiErr}), but so did REST (${restErr}); waiting for REST`);
    return;
  }

  failures++;
  console.log(`[watchdog] SSR check failed (${uiErr}) while REST is up: ${failures}/${FAILURES_BEFORE_RESTART}`);
  if (failures >= FAILURES_BEFORE_RESTART) {
    // pm2-runtime is PID 1, so stopping it stops the container, and compose's
    // "restart: always" brings it back
    console.log('[watchdog] stopping pm2 so the container restarts');
    process.kill(1, 'SIGTERM');
  }
}

async function loop() {
  await check();
  setTimeout(loop, INTERVAL_MS);
}

setTimeout(loop, INTERVAL_MS);
