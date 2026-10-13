// Scenario 5 — soak / sustained mixed load.
//
// The other scenarios are bursts. This one answers a different question: does
// anything DRIFT under sustained traffic? Three things are watched:
//
//   * latency across the run — a slow leak in the pool or a growing queue shows
//     up as p95 rising in the later stages, not in the average.
//   * correctness at the end — the wallet reopened and reconciled after the
//     whole run, which is the same assertion as hot-wallet but over hours
//     instead of seconds.
//   * outbox drain — the service exposes wallet_outbox_pending; if it grows
//     while the system is idle at the end of the run, the relay is losing.
//
// Default duration is short so it is usable in a local run; scale it up with
// -e DURATION=30m for a real soak.
import { check, fail } from 'k6';
import exec from 'k6/execution';
import http from 'k6/http';
import { Counter, Gauge } from 'k6/metrics';
import { textSummary } from '../lib/summary.js';
import { cfg } from '../lib/config.js';
import { internalToken, providerToken } from '../lib/auth.js';
import {
  decimalToMinor,
  getWallet,
  minorToDecimal,
  openWallet,
  reconcile,
  submit,
  uniq,
  uuid,
} from '../lib/wallet.js';
import { correctness, moneyLatency, soakThresholds } from '../lib/thresholds.js';

export const appliedCount = new Counter('soak_applied');
export const outboxPending = new Gauge('soak_outbox_pending');

const VUS = Number(__ENV.VUS || 16);
const DURATION = __ENV.DURATION || '2m';
const WALLETS = Number(__ENV.WALLETS || 64);
const POLL_EVERY = Number(__ENV.POLL_EVERY || 10); // iterations between metrics polls

export const options = {
  scenarios: {
    soak: {
      executor: 'constant-vus',
      vus: VUS,
      duration: DURATION,
      gracefulStop: '10s',
    },
  },
  thresholds: Object.assign({}, correctness, soakThresholds),
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  const internal = internalToken();
  const run = `${Date.now()}`;
  // Big seed: the soak does not know in advance how many operations will land,
  // so the balance must survive an unbounded-ish number of debits.
  const seed = '10000000.00';
  const wallets = [];
  for (let i = 0; i < WALLETS; i++) {
    const player = uuid();
    const opened = openWallet(cfg.api, internal, player, seed, `soak-open-${run}-${i}`);
    if (!opened.body.id) {
      fail(`setup: wallet ${i} failed: ${opened.status} ${JSON.stringify(opened.body)}`);
    }
    wallets.push({ id: opened.body.id, player, moved: 0 });
  }
  return { run, wallets };
}

export default function (data) {
  const vu = exec.vu.idInTest;
  const it = exec.vu.iterationInInstance;
  const wallet = data.wallets[(vu + it) % data.wallets.length];
  const provider = providerToken();

  const ext = uniq(`lt-soak-${data.run}`, `${vu}`);
  const res = submit(
    cfg.api,
    provider,
    {
      externalTransactionId: ext,
      playerId: wallet.player,
      walletId: wallet.id,
      roundId: `soak-round-${vu}`,
      kind: 'BET',
      amount: cfg.amount,
    },
    `provider-a:${ext}`
  );
  moneyLatency.add(res.timings.duration);

  if ((res.status === 200 || res.status === 201) && res.body.status === 'PROCESSED') {
    appliedCount.add(1);
  }

  check(res, { 'soak submit is 2xx': (r) => r.status === 200 || r.status === 201 });

  // Occasionally sample the outbox backlog. It is a gauge, so k6 records the
  // trend and the summary shows whether it was growing without bound.
  if (it % POLL_EVERY === 0) {
    const metrics = httpGetMetrics();
    const pending = /wallet_outbox_pending ([\d.]+)/.exec(metrics);
    if (pending) {
      outboxPending.add(Number(pending[1]));
    }
  }
}

function httpGetMetrics() {
  const res = http.get(`${cfg.api}/metrics`, { tags: { name: 'metrics' } });
  return res.body || '';
}

export function teardown(data) {
  const internal = internalToken();
  // Reconcile every soak wallet: under sustained traffic the ledger must still
  // sum to the balance for each one.
  let totalMoved = 0;
  for (const w of data.wallets) {
    const rec = reconcile(cfg.api, internal, w.id);
    if (rec.status !== 200) {
      fail(`soak: reconcile ${w.id} returned ${rec.status}`);
    }
    if (rec.body.consistent !== true) {
      fail(
        `SOAK DRIFT on ${w.id}: stored ${rec.body.storedBalance.amount} vs calculated ` +
          `${rec.body.calculatedBalance.amount} — the balance drifted during sustained load`
      );
    }
    const ledger = getWallet(cfg.api, internal, w.id);
    if (ledger.status !== 200) {
      fail(`soak: GET ${w.id} returned ${ledger.status}`);
    }
    totalMoved += rec.body.checkedEntries;
  }
  console.log(`soak OK — ${data.wallets.length} wallets reconciled, ${totalMoved} ledger entries total, all consistent`);
}

function playerFor(run, i) {
  const tail = String(i).padStart(12, '0');
  const head = run.slice(-8).padStart(8, '0');
  return `${head}-${String(i % 10000).padStart(4, '0')}-7666-8666-${tail}`;
}

export function handleSummary(data) {
  return {
    stdout: textSummary(data, { title: 'soak' }),
  };
}
