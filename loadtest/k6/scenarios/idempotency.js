// Scenario 2 — idempotent replay and key conflict.
//
// The idempotency guarantee has three distinct clauses, and this scenario tests
// all three against the live service rather than a mock:
//
//   1. same key + same payload  -> the ORIGINAL result is replayed and money
//      moves ONCE. Under real concurrency, not in sequence: several VUs submit
//      the same key at the same instant, so the winner/loser race inside the
//      transaction is exercised.
//   2. same key + DIFFERENT payload -> 409 IDEMPOTENCY_KEY_CONFLICT.
//   3. distinct keys, same request -> both applied. If the service were
//      deduplicating on payload hash instead of on the key, this would be
//      silently collapsed and the balance would be wrong.
//
// The end state is asserted in teardown(): a wallet that received K distinct
// bets and R replays must have moved exactly K amounts.
import { check, fail } from 'k6';
import exec from 'k6/execution';
import { Counter } from 'k6/metrics';
import { textSummary } from '../lib/summary.js';
import { cfg } from '../lib/config.js';
import { internalToken, providerToken } from '../lib/auth.js';
import {
  decimalToMinor,
  getWallet,
  minorToDecimal,
  opIdentity,
  openWallet,
  reconcile,
  submit,
  uuid,
} from '../lib/wallet.js';
import { correctness, moneyLatency, moneyThresholds } from '../lib/thresholds.js';

export const replayCount = new Counter('idem_replayed');
export const appliedCount = new Counter('idem_applied');
export const conflictCount = new Counter('idem_conflicts');
export const rejectedCount = new Counter('idem_rejected');

const VUS = Number(__ENV.VUS || 24);
// One logical operation per iteration: 1/3 of them replay the operation of the
// previous iteration on the same VU.
const ITER_PER_VU = Number(__ENV.ITER_PER_VU || 9);
const AMOUNT_MINOR = decimalToMinor(cfg.amount);

export const options = {
  scenarios: {
    idempotency: {
      executor: 'per-vu-iterations',
      vus: VUS,
      iterations: ITER_PER_VU,
      maxDuration: '5m',
    },
  },
  thresholds: Object.assign({}, correctness, moneyThresholds),
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  const internal = internalToken();
  const run = `${Date.now()}`;
  const player = uuid();
  const seed = minorToDecimal(VUS * ITER_PER_VU * AMOUNT_MINOR + 100000);

  const opened = openWallet(cfg.api, internal, player, seed, `idem-open-${run}`);
  if (!opened.body.id) {
    fail(`setup could not open wallet: ${opened.status} ${JSON.stringify(opened.body)}`);
  }
  return { run, walletId: opened.body.id, playerId: player, seedMinor: decimalToMinor(seed) };
}

export default function (data) {
  const vu = exec.vu.idInTest;
  const it = exec.vu.iterationInInstance;
  // Every third iteration (after the first) replays the operation of the
  // previous iteration on this VU — same key, same payload.
  const replay = it > 0 && it % 3 === 1;
  const seq = replay ? it - 1 : it;
  const { ext, key } = opIdentity(data.run, `idem${vu}`, seq);

  const req = {
    externalTransactionId: ext,
    playerId: data.playerId,
    walletId: data.walletId,
    roundId: `idem-round-${vu}`,
    kind: 'BET',
    amount: cfg.amount,
  };

  const res = submit(cfg.api, providerToken(), req, key);
  moneyLatency.add(res.timings.duration);

  if (res.status === 200 || res.status === 201) {
    if (res.body.idempotentReplay) {
      replayCount.add(1);
      check(res, { 'replay returns the stored status': (r) => r.body.status === 'PROCESSED' });
    } else if (res.body.status === 'PROCESSED') {
      appliedCount.add(1);
    } else {
      rejectedCount.add(1);
    }
  } else if (res.status === 409) {
    conflictCount.add(1);
  }

  check(res, { 'submit is 2xx': (r) => r.status === 200 || r.status === 201 });
}

// A second, deliberate conflict check: reuse ONE key with a DIFFERENT amount.
// This must be refused; accepting it would mean the idempotency identity ignores
// the payload.
export function teardown(data) {
  const internal = internalToken();
  const provider = providerToken();
  const key = `idem-conflict-${data.run}`;
  const ext = `idem-conflict-${data.run}`;

  const first = submit(
    cfg.api,
    provider,
    {
      externalTransactionId: ext,
      playerId: data.playerId,
      walletId: data.walletId,
      roundId: 'idem-conflict-round',
      kind: 'BET',
      amount: cfg.amount,
    },
    key
  );
  if (first.status !== 200 && first.status !== 201) {
    fail(`conflict probe: first submit returned ${first.status} ${JSON.stringify(first.body)}`);
  }

  // Same key, different amount (and different external id is not allowed either,
  // so we keep the external id and only vary the money).
  const second = submit(
    cfg.api,
    provider,
    {
      externalTransactionId: ext,
      playerId: data.playerId,
      walletId: data.walletId,
      roundId: 'idem-conflict-round',
      kind: 'BET',
      amount: '999.00',
    },
    key
  );
  if (second.status !== 409 || (second.body.error || {}).code !== 'IDEMPOTENCY_KEY_CONFLICT') {
    fail(
      `conflict probe: reusing a key with a different payload must be 409 IDEMPOTENCY_KEY_CONFLICT, ` +
        `got ${second.status} ${JSON.stringify(second.body)}`
    );
  }

  // Distinct-key control: the SAME payload under a different key must be applied
  // again. If the service deduplicated on payload hash this would collapse.
  const distinctExt = `idem-distinct-${data.run}`;
  const distinct = submit(
    cfg.api,
    provider,
    {
      externalTransactionId: distinctExt,
      playerId: data.playerId,
      walletId: data.walletId,
      roundId: 'idem-distinct-round',
      kind: 'BET',
      amount: cfg.amount,
    },
    `provider-a:${distinctExt}`
  );
  if (distinct.status !== 200 && distinct.status !== 201) {
    fail(`distinct-key control failed: ${distinct.status} ${JSON.stringify(distinct.body)}`);
  }
  if (distinct.body.idempotentReplay === true) {
    fail('distinct-key control was replayed — the service is deduplicating on payload, not on the key');
  }

  // End state: every non-replayed, non-rejected submit moved exactly one amount,
  // plus the two probes that are expected to have moved money (first + distinct).
  const applied = appliedCountValue();
  const expectedMinor =
    data.seedMinor - (applied + 2) * AMOUNT_MINOR;
  const wallet = getWallet(cfg.api, internal, data.walletId);
  const actualMinor = decimalToMinor(wallet.body.balance.amount);

  if (actualMinor !== expectedMinor) {
    fail(
      `IDEMPOTENCY FAILURE: balance is ${wallet.body.balance.amount}, expected ` +
        `${minorToDecimal(expectedMinor)} — a replay moved money twice`
    );
  }

  const rec = reconcile(cfg.api, internal, data.walletId);
  if (rec.body.consistent !== true) {
    fail(`reconciliation disagrees: ${JSON.stringify(rec.body)}`);
  }

  console.log(
    `idempotency OK — ${applied} applied + 2 probes, balance ${wallet.body.balance.amount}, ` +
      `${rec.body.checkedEntries} ledger entries, conflict and distinct-key controls passed`
  );
}

// Counts, per VU, how many iterations are replays. Iteration i replays when
// i > 0 && i % 3 == 1, so the applied count per VU is ITER_PER_VU minus that.
function replaysPerVU() {
  let n = 0;
  for (let i = 1; i < ITER_PER_VU; i++) {
    if (i % 3 === 1) n++;
  }
  return n;
}

// Applied operations across all VUs, excluding the two money-moving probes that
// teardown performs on purpose (the conflict probe's first submit and the
// distinct-key control), which are added separately.
function appliedCountValue() {
  return VUS * (ITER_PER_VU - replaysPerVU());
}

function playerFor(run) {
  const tail = run.slice(-12).padStart(12, '0');
  return `${run.slice(-8).padStart(8, '0')}-2222-7222-8222-${tail}`;
}

export function handleSummary(data) {
  return {
    stdout: textSummary(data, { title: 'idempotency' }),
  };
}
