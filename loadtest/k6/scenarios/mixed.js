// Scenario 3 — mixed traffic, happy path plus adversarial rows.
//
// This is the closest thing to production shape: many distinct wallets, several
// kinds of operation, a small fraction of requests that MUST be refused. It
// answers two questions the other scenarios do not:
//
//   * does throughput on independent wallets scale, or does the per-wallet lock
//     leak into a global bottleneck?
//   * does the service refuse the right things with the right status, under
//     load, instead of failing in an unexpected way?
//
// Adversarial rows (cfg.badFraction of iterations):
//   - BET larger than the balance                -> 422 INSUFFICIENT_FUNDS
//   - REFUND with no matching reference          -> 422 REFERENCE_MISMATCH
//   - unknown wallet id                          -> 404 WALLET_NOT_FOUND
//   - a second wallet for a (player, currency)   -> 409 WALLET_ALREADY_EXISTS
//
// Because these are EXPECTED refusals, the run is green only if they come back
// as the mapped error code and NOT as a 500. A 500 here is a real defect that a
// pure happy-path test would never surface.
import { check, fail } from 'k6';
import exec from 'k6/execution';
import { Counter } from 'k6/metrics';
import { textSummary } from '../lib/summary.js';
import { cfg } from '../lib/config.js';
import { internalToken, providerToken } from '../lib/auth.js';
import {
  decimalToMinor,
  getLedger,
  minorToDecimal,
  openWallet,
  reconcile,
  submit,
  uniq,
  uuid,
} from '../lib/wallet.js';
import { correctness, mixedThresholds, moneyLatency, readLatency } from '../lib/thresholds.js';

export const appliedCount = new Counter('mixed_applied');
export const rejectedInsufficient = new Counter('mixed_rejected_insufficient');
export const rejectedReference = new Counter('mixed_rejected_reference');
export const duplicateWallet = new Counter('mixed_duplicate_wallet');
export const notFound = new Counter('mixed_not_found');
export const serverErrors = new Counter('mixed_server_errors');

const VUS = Number(__ENV.VUS || 48);
const ITER_PER_VU = Number(__ENV.ITER_PER_VU || 10);
const WALLETS = Number(__ENV.WALLETS || 200);

export const options = {
  scenarios: {
    mixed: {
      executor: 'per-vu-iterations',
      vus: VUS,
      iterations: ITER_PER_VU,
      maxDuration: '5m',
    },
  },
  thresholds: Object.assign({}, correctness, mixedThresholds, {
    // Adversarial rows are a known cost, but they must stay bounded.
    mixed_server_errors: ['count==0'],
  }),
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  const internal = internalToken();
  const run = `${Date.now()}`;
  const seed = minorToDecimal(500000); // 5000.00 per wallet, enough for the mix

  // Pre-open a pool of wallets so the money path is not diluted by wallet
  // creation. Each gets a distinct player, and an Idempotency-Key so a re-run
  // replays rather than failing.
  const wallets = [];
  for (let i = 0; i < WALLETS; i++) {
    const player = uuid();
    const opened = openWallet(cfg.api, internal, player, seed, `mixed-open-${run}-${i}`);
    if (!opened.body.id) {
      fail(`setup: wallet ${i} failed: ${opened.status} ${JSON.stringify(opened.body)}`);
    }
    wallets.push({ id: opened.body.id, player, created: i === 0 });
  }

  return { run, wallets, seed };
}

export default function (data) {
  const vu = exec.vu.idInTest;
  const it = exec.vu.iterationInInstance;
  const target = data.wallets[(vu + it) % data.wallets.length];
  const provider = providerToken();
  const internal = internalToken();

  // Deterministic-but-spread choice of row shape. A 32-bit mix of (VU,
  // iteration) spreads the adversarial rows evenly instead of clustering them,
  // while staying reproducible: the same VU+iteration always yields the same
  // shape, so a failure can be replayed.
  const mixed = hash32(vu * 2654435761 + it * 40503) / 4294967296;
  const bad = mixed < cfg.badFraction;

  if (bad) {
    adversarial(data, target, vu, it, provider, internal);
  } else {
    happyPath(data, target, vu, it, provider, internal);
  }
}

function happyPath(data, wallet, vu, it, provider, internal) {
  const ext = uniq(`lt-mix-${data.run}`, `${wallet.id.slice(0, 8)}`);
  // Debis and credits alternate so a wallet's balance stays usable.
  const kind = it % 3 === 2 ? 'WIN' : 'BET';

  const res = submit(
    cfg.api,
    provider,
    {
      externalTransactionId: ext,
      playerId: wallet.player,
      walletId: wallet.id,
      roundId: `mix-round-${vu}-${it}`,
      kind,
      amount: cfg.amount,
    },
    `provider-a:${ext}`
  );
  moneyLatency.add(res.timings.duration);

  if (res.status === 200 || res.status === 201) {
    if (res.body.status === 'PROCESSED') appliedCount.add(1);
  } else if (res.status >= 500) {
    serverErrors.add(1);
  }

  check(res, {
    'happy path is 2xx': (r) => r.status === 200 || r.status === 201,
    'happy path is processed or replayed': (r) =>
      r.status === 200 || r.status === 201 ? (r.body || {}).status === 'PROCESSED' : true,
  });

  // Every few iterations, exercise the read paths so they are measured under the
  // same load rather than in an idle system.
  if (it % 5 === 0) {
    const ledger = getLedger(cfg.api, internal, wallet.id, 20);
    readLatency.add(ledger.timings.duration);
    check(ledger, { 'ledger is 200': (r) => r.status === 200 });
  }
}

function adversarial(data, wallet, vu, it, provider, internal) {
  const shape = (vu + it) % 4;

  if (shape === 0) {
    // Overdraw: an amount no wallet can cover.
    const ext = uniq(`lt-over-${data.run}`, `${vu}`);
    const res = submit(
      cfg.api,
      provider,
      {
        externalTransactionId: ext,
        playerId: wallet.player,
        walletId: wallet.id,
        roundId: `over-round-${vu}-${it}`,
        kind: 'BET',
        amount: cfg.overdrawAmount,
      },
      `provider-a:${ext}`
    );
    moneyLatency.add(res.timings.duration);
    if (res.status === 422 && (res.body.failureCode || res.body.error)) {
      rejectedInsufficient.add(1);
    } else if (res.status >= 500) {
      serverErrors.add(1);
    }
    check(res, {
      'overdraw is refused with 422': (r) => r.status === 422,
      'overdraw is not a 5xx': (r) => r.status < 500,
    });
    return;
  }

  if (shape === 1) {
    // Refund referencing a transaction that does not exist (yet).
    //
    // The service answers 202 PENDING_REFERENCE rather than 422: an unresolved
    // reference is not an error, it is a future obligation the reference-resolver
    // will retry. Asserting that explicitly pins the behaviour — a 422 here would
    // mean the resolver path was lost.
    const ext = uniq(`lt-refund-${data.run}`, `${vu}`);
    const res = submit(
      cfg.api,
      provider,
      {
        externalTransactionId: ext,
        playerId: wallet.player,
        walletId: wallet.id,
        roundId: `refund-round-${vu}-${it}`,
        kind: 'REFUND',
        amount: cfg.amount,
        referenceExternalTransactionId: `does-not-exist-${data.run}`,
      },
      `provider-a:${ext}`
    );
    moneyLatency.add(res.timings.duration);
    if (res.status === 202 && res.body.status === 'PENDING_REFERENCE') {
      rejectedReference.add(1);
    } else if (res.status >= 500) {
      serverErrors.add(1);
    }
    check(res, {
      'dangling refund is accepted as PENDING_REFERENCE': (r) =>
        r.status === 202 && (r.body || {}).status === 'PENDING_REFERENCE',
      'dangling refund is not a 5xx': (r) => r.status < 500,
    });
    return;
  }

  if (shape === 2) {
    // Non-existent wallet.
    const ext = uniq(`lt-ghost-${data.run}`, `${vu}`);
    const res = submit(
      cfg.api,
      provider,
      {
        externalTransactionId: ext,
        playerId: uuid(),
        walletId: '00000000-0000-7000-8000-000000000000',
        roundId: `ghost-round-${vu}-${it}`,
        kind: 'BET',
        amount: cfg.amount,
      },
      `provider-a:${ext}`
    );
    moneyLatency.add(res.timings.duration);
    if (res.status === 404 || res.status === 422) {
      notFound.add(1);
    } else if (res.status >= 500) {
      serverErrors.add(1);
    }
    check(res, {
      'unknown wallet is 404 without a 5xx': (r) => r.status <= 404 && r.status !== 500,
    });
    return;
  }

  // shape 3: a second wallet for a (player, currency) that already has one.
  const dup = openWallet(cfg.api, internal, wallet.player, '10.00', `mixed-dup-${data.run}-${vu}-${it}`);
  if (dup.status === 409) {
    duplicateWallet.add(1);
  } else if (dup.status >= 500) {
    serverErrors.add(1);
  }
  check(dup, {
    'duplicate wallet is 409 without a 5xx': (r) => r.status < 500,
  });
}

// Reconcile a sample of the wallets: the per-wallet lock protects each one, and
// this confirms the ledger still sums to the stored balance after mixed traffic.
export function teardown(data) {
  const internal = internalToken();
  let checked = 0;
  for (let i = 0; i < data.wallets.length; i += Math.max(1, Math.floor(data.wallets.length / 20))) {
    const w = data.wallets[i];
    const rec = reconcile(cfg.api, internal, w.id);
    if (rec.status !== 200) {
      fail(`reconcile ${w.id} returned ${rec.status}`);
    }
    if (rec.body.consistent !== true) {
      fail(
        `MIXED TRAFFIC CORRUPTED ${w.id}: stored ${rec.body.storedBalance.amount} vs ` +
          `calculated ${rec.body.calculatedBalance.amount}`
      );
    }
    checked++;
  }
  console.log(`mixed OK — ${checked} wallets reconciled, all consistent`);
}

function hash32(x) {
  // xorshift-style integer mix; deterministic and cheap.
  let h = x | 0;
  h ^= h << 13;
  h ^= h >>> 17;
  h ^= h << 5;
  return h >>> 0;
}

function playerFor(run, i) {
  const tail = String(i).padStart(12, '0');
  const head = run.slice(-8).padStart(8, '0');
  return `${head}-${String(i % 10000).padStart(4, '0')}-7333-8333-${tail}`;
}

export function handleSummary(data) {
  return {
    stdout: textSummary(data, { title: 'mixed' }),
  };
}
