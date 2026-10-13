// Scenario 1 — hot wallet / row-level lock.
//
// ONE wallet, many VUs, all writes serialize on the same row lock
// (SELECT ... FOR UPDATE). This is the worst case for the per-wallet lock and
// the one that exposes:
//
//   * lost updates         — two concurrent debits reading the same
//                            balance_before, so the final balance is wrong
//   * negative balance     — CHECK (balance_minor >= 0) must never fire
//   * a version that is not a faithful operation count
//
// The contract is deliberately simple to reason about: every applied operation
// moves EXACTLY `cfg.amount` (BET debits, WIN credits), replays return the
// stored result and move nothing, and the seed is large enough that no BET is
// ever rejected. So after the run the balance MUST equal
// `seed - totalDebits + totalCredits` to the cent, and the version MUST equal
// `applied + 1` (the opening row is version 1).
//
// That end-state check lives in teardown() and is the point of the scenario: it
// is an assertion about the database, not about HTTP status codes.
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
import { correctness, hotWalletThresholds, moneyLatency } from '../lib/thresholds.js';

export const appliedCount = new Counter('hot_applied');
export const replayCount = new Counter('hot_replayed');
export const conflictCount = new Counter('hot_conflicts');

const VUS = Number(__ENV.VUS || 48);
const ITER_PER_VU = Number(__ENV.ITER_PER_VU || 7);
const AMOUNT_MINOR = decimalToMinor(cfg.amount);

export const options = {
  scenarios: {
    hot: {
      executor: 'per-vu-iterations',
      vus: VUS,
      iterations: ITER_PER_VU,
      maxDuration: '5m',
    },
  },
  thresholds: Object.assign({}, correctness, hotWalletThresholds),
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  const internal = internalToken();
  const provider = providerToken();
  const run = `${Date.now()}`;
  const player = uuid();

  // Seed covers every debit with headroom, which is what makes "no forced
  // rejection" a legitimate expectation below.
  const seedMinor = VUS * ITER_PER_VU * AMOUNT_MINOR + 100000;
  const seed = minorToDecimal(seedMinor);

  const opened = openWallet(cfg.api, internal, player, seed, `hot-open-${run}`);
  if (!opened.body.id) {
    fail(`setup could not open wallet: ${opened.status} ${JSON.stringify(opened.body)}`);
  }

  return { run, walletId: opened.body.id, playerId: player, seedMinor };
}

export default function (data) {
  const vu = exec.vu.idInTest;
  const it = exec.vu.iterationInInstance;
  // Alternating debit/credit keeps the balance oscillating near the seed, so it
  // never approaches either bound.
  const kind = it % 2 === 0 ? 'BET' : 'WIN';
  const { ext, key } = opIdentity(data.run, `hot${vu}`, it);

  const res = submit(
    cfg.api,
    providerToken(),
    {
      externalTransactionId: ext,
      playerId: data.playerId,
      walletId: data.walletId,
      roundId: `hot-round-${vu}`,
      kind,
      amount: cfg.amount,
    },
    key
  );

  moneyLatency.add(res.timings.duration);

  if (res.status === 200 || res.status === 201) {
    if (res.body.idempotentReplay) {
      replayCount.add(1);
    } else if (res.body.status === 'PROCESSED') {
      appliedCount.add(1);
    }
  } else if (res.status === 409) {
    // Impossible by construction: every (VU, iteration) has a distinct key. If
    // this ever fires, keys are colliding — a generator bug, not a service bug,
    // and the run must fail loudly rather than report a green latency.
    conflictCount.add(1);
  }

  check(res, {
    'submit is 2xx': (r) => r.status === 200 || r.status === 201,
    'funded wallet never rejects': (r) => r.status !== 422,
  });
}

// teardown() runs once, after every VU has stopped, and is where this scenario
// actually asserts. k6 reports a thrown error here as a failed run.
export function teardown(data) {
  const internal = internalToken();

  const wallet = getWallet(cfg.api, internal, data.walletId);
  if (wallet.status !== 200) {
    fail(`teardown: GET wallet returned ${wallet.status}`);
  }

  // Recompute the expected end state from the operation mix: with an even number
  // of iterations per VU, debits and credits cancel out.
  const perVU = ITER_PER_VU;
  const debits = Math.ceil(perVU / 2) * VUS;
  const credits = Math.floor(perVU / 2) * VUS;
  const expectedMinor = data.seedMinor - debits * AMOUNT_MINOR + credits * AMOUNT_MINOR;

  const actualMinor = decimalToMinor(wallet.body.balance.amount);

  if (actualMinor !== expectedMinor) {
    fail(
      `LOCK FAILURE: balance is ${wallet.body.balance.amount}, expected ${minorToDecimal(expectedMinor)} ` +
        `(seed ${minorToDecimal(data.seedMinor)}, ${debits} debits, ${credits} credits) — a lost update slipped through`
    );
  }

  const expectedVersion = debits + credits + 1; // +1 for the OPENING row
  if (wallet.body.version !== expectedVersion) {
    fail(`LOCK FAILURE: version is ${wallet.body.version}, expected ${expectedVersion}`);
  }

  // The service's own reconciliation must agree with our arithmetic. checkedEntries
  // counts the OPENING entry too (the initial balance is itself a ledger row),
  // so it is one more than the operations we applied.
  const rec = reconcile(cfg.api, internal, data.walletId);
  if (rec.status !== 200 || rec.body.consistent !== true) {
    fail(`reconciliation disagrees: ${rec.status} ${JSON.stringify(rec.body)}`);
  }
  if (rec.body.checkedEntries !== debits + credits + 1) {
    fail(
      `reconciliation checked ${rec.body.checkedEntries} entries, expected ${debits + credits + 1} ` +
        `(including the OPENING row)`
    );
  }

  console.log(
    `hot-wallet OK — balance ${wallet.body.balance.amount}, version ${wallet.body.version}, ` +
      `${rec.body.checkedEntries} ledger entries, reconciliation consistent`
  );
}

function playerFor(run) {
  // Any RFC4122-ish string; the service stores it as uuid.
  const tail = run.slice(-12).padStart(12, '0');
  return `${run.slice(-8).padStart(8, '0')}-1111-7111-8111-${tail}`;
}

export function handleSummary(data) {
  return {
    stdout: textSummary(data, { title: 'hot-wallet' }),
  };
}
