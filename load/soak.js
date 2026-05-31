/**
 * forge 24 h soak test — pre-release gate, NOT run in nightly CI.
 *
 * Run manually before a GA release:
 *   FORGE_BASE=http://prod-forge:8080 k6 run load/soak.js
 *
 * Asserts §6 SLOs hold over a sustained 24-hour window:
 *   - Cached metadata GET p99 < 50 ms throughout (no latency drift).
 *   - Publish error rate < 0.1 %.
 *   - No memory growth detectable via /metrics (check Grafana dashboard
 *     manually; k6 cannot directly assert Go runtime heap growth).
 *
 * The low VU count (5 readers, 2 writers) simulates steady background load
 * rather than a spike — the goal is drift detection, not peak capacity.
 *
 * For chaos / HA tests (kill a node mid-run) add SIGKILL to the app pod
 * while this script is running and verify the error rate stays at zero
 * after pod restart.
 */

import http from 'k6/http';
import { check, sleep } from 'k6';
import { b64encode } from 'k6/encoding';

const BASE = __ENV.FORGE_BASE || 'http://localhost:8080';
const NPM = `${BASE}/repository/npm-hosted`;

export const options = {
  scenarios: {
    sustained_reads: {
      exec: 'sustainedReads',
      executor: 'constant-vus',
      vus: 5,
      duration: '24h',
      tags: { scenario: 'sustained_reads' },
    },
    sustained_writes: {
      exec: 'sustainedWrites',
      executor: 'constant-vus',
      vus: 2,
      duration: '24h',
      tags: { scenario: 'sustained_writes' },
    },
  },

  thresholds: {
    'http_req_duration{scenario:sustained_reads}': ['p(99)<50'],
    'http_req_failed{scenario:sustained_reads}': ['rate<0.001'],
    'http_req_failed{scenario:sustained_writes}': ['rate<0.001'],
  },
};

let publishCounter = 0;

export function setup() {
  // Pre-publish a package for sustained reads.
  publishNPM('soak-read-pkg', '1.0.0');
  return {};
}

export function sustainedReads() {
  const res = http.get(`${NPM}/soak-read-pkg`);
  check(res, { 'packument 200': (r) => r.status === 200 });
  sleep(1);
}

export function sustainedWrites() {
  publishCounter++;
  const name = `soak-write-${__VU}-${publishCounter}`;
  const res = publishNPM(name, '1.0.0');
  check(res, { 'publish ok': (r) => r.status === 201 || r.status === 200 });
  sleep(5);
}

function publishNPM(name, version) {
  const body = JSON.stringify({
    name,
    versions: {
      [version]: { name, version, dist: { shasum: 'deadbeef' } },
    },
    'dist-tags': { latest: version },
    _attachments: {
      [`${name}-${version}.tgz`]: {
        content_type: 'application/octet-stream',
        data: b64encode(`fake-tarball-${name}`),
      },
    },
  });
  return http.put(`${NPM}/${name}`, body, {
    headers: { 'Content-Type': 'application/json' },
  });
}
