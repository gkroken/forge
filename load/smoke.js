/**
 * forge load smoke test — nightly CI gate.
 *
 * Two scenarios run sequentially:
 *
 *   metadata_get (0s–60s): 10 VUs continuously read a cached npm packument.
 *     SLO: p99 latency < 50ms, error rate < 1%.
 *
 *   concurrent_publish (70s–130s): 50 VUs each publish one unique npm package
 *     in parallel. SLO: zero failures.
 *
 *   index_verify (140s): 1 VU reads back all 50 published packuments to confirm
 *     no index updates were lost under concurrent write load.
 *     SLO: 100% of checks pass.
 *
 * Run:
 *   k6 run load/smoke.js
 *   FORGE_BASE=http://host:8080 k6 run load/smoke.js
 */

import http from 'k6/http';
import { check } from 'k6';
import { b64encode } from 'k6/encoding';
import { vu } from 'k6/execution';

const BASE = __ENV.FORGE_BASE || 'http://localhost:8080';
const NPM = `${BASE}/repository/npm-hosted`;

export const options = {
  scenarios: {
    metadata_get: {
      exec: 'metadataGet',
      executor: 'constant-vus',
      vus: 10,
      duration: '60s',
      tags: { scenario: 'metadata_get' },
    },
    concurrent_publish: {
      exec: 'concurrentPublish',
      executor: 'shared-iterations',
      vus: 50,
      iterations: 50,
      startTime: '70s',
      maxDuration: '60s',
      tags: { scenario: 'concurrent_publish' },
    },
    index_verify: {
      exec: 'indexVerify',
      executor: 'shared-iterations',
      vus: 1,
      iterations: 1,
      startTime: '140s',
      maxDuration: '30s',
      tags: { scenario: 'index_verify' },
    },
  },

  thresholds: {
    // §6 SLO: cached metadata GET p99 < 50 ms.
    'http_req_duration{scenario:metadata_get}': ['p(99)<50'],
    // No errors during warm reads.
    'http_req_failed{scenario:metadata_get}': ['rate<0.01'],
    // §6 SLO: 50 concurrent publishes with zero index loss.
    'http_req_failed{scenario:concurrent_publish}': ['rate==0'],
    // All 50 packuments must be readable after concurrent publish.
    'checks{scenario:index_verify}': ['rate==1'],
  },
};

// setup runs once before all scenarios. Publishes the base packument that
// metadata_get will read throughout its 60-second run.
export function setup() {
  const name = 'smoke-baseline-pkg';
  const res = publishNPM(name, '1.0.0');
  if (res.status !== 201 && res.status !== 200) {
    console.error(`setup: publish failed: ${res.status} ${res.body}`);
  }
  return { basePkg: name };
}

// metadataGet reads the base packument repeatedly. Tagged as metadata_get so
// thresholds are scoped to this scenario.
export function metadataGet(data) {
  const res = http.get(`${NPM}/${data.basePkg}`, {
    tags: { scenario: 'metadata_get' },
  });
  check(res, {
    'packument 200': (r) => r.status === 200,
    'content-type json': (r) =>
      (r.headers['Content-Type'] || '').includes('application/json'),
  });
}

// concurrentPublish: each VU publishes one uniquely-named package.
// vu.idInScenario is 0-indexed and scenario-local, so packages are
// smoke-concurrent-1 … smoke-concurrent-50 regardless of global VU allocation.
export function concurrentPublish() {
  const name = `smoke-concurrent-${vu.idInScenario + 1}`;
  const res = publishNPM(name, '1.0.0');
  check(res, { 'publish ok': (r) => r.status === 201 || r.status === 200 });
}

// indexVerify reads back all 50 packuments and asserts each is present.
// Runs once after concurrent_publish finishes.
export function indexVerify() {
  for (let i = 1; i <= 50; i++) {
    const name = `smoke-concurrent-${i}`;
    const res = http.get(`${NPM}/${name}`, { tags: { scenario: 'index_verify' } });
    check(res, {
      [`packument smoke-concurrent-${i} visible`]: (r) => r.status === 200,
    });
  }
}

// publishNPM publishes a minimal npm package to npm-hosted.
function publishNPM(name, version) {
  const tarball = b64encode(`fake-tarball-${name}-${version}`);
  const body = JSON.stringify({
    name,
    versions: {
      [version]: { name, version, dist: { shasum: 'deadbeef' } },
    },
    'dist-tags': { latest: version },
    _attachments: {
      [`${name}-${version}.tgz`]: {
        content_type: 'application/octet-stream',
        data: tarball,
      },
    },
  });
  return http.put(`${NPM}/${name}`, body, {
    headers: { 'Content-Type': 'application/json' },
  });
}
