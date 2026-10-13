// Scenario 4 — distributed load across replicas.
//
// Points the same hot-wallet contract at several API instances at once
// (compose --profile multi on :8081/:8082/:8083, or three kind replicas behind
// the NodePorts). The invariant is the interesting part:
//
//   The per-wallet lock must hold ACROSS PROCESSES, not only inside one. If the
//   lock were a Go mutex or an in-process cache, this scenario would corrupt the
//   balance while the single-instance scenario stayed green — which is exactly
//   the bug the row-level lock and the optimistic `version` exist to prevent.
//
// The end-state assertion is deliberately identical to hot-wallet.js: same
// arithmetic, so a green run here means the distributed topology produces the
// same balance as the single-instance one.
import { check, fail } from 'k6';
import exec from 'k6/execution';
import { Counter } from 'k6/metrics';
import { textSummary } from '../lib/summary.js';
import { cfg, pickTarget } from '../lib/config.js';
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
import { correctness, distributedThresholds, moneyLatency } from '../lib/thresholds.js';

export const appliedCount = new Counter('dist_applied');
export const conflictCount = new Counter('dist_conflicts');

const VUS = Number(__ENV.VUS || 96);
const ITER_PER_VU = Number(__ENV.ITER_PER_VU || 6);
const AMOUNT_MINOR = decimalToMinor(cfg.amount);

export const options = {
  scenarios: {
    distributed: {
      executor: 'per-vu-iterations',
      vus: VUS,
      iterations: ITER_PER_VU,
      maxDuration: '5m',
    },
  },
  thresholds: Object.assign({}, correctness, distributedThresholds, {
    dist_conflicts: ['count==0'],
  }),
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  if (cfg.targets.length < 2) {
    // Not fatal: running against one instance is still a valid (weaker) test,
    // but the operator should know the run does not prove the distributed case.
    console.warn(
      `distributed: only ${cfg.targets.length} target(s) configured — set APIS=http://host:8081,http://host:8082,... ` +
        `to actually exercise cross-process locking`
    );
  }
  const internal = internalToken();
  const run = `${Date.now()}`;
  const player = uuid();
  const seedMinor = VUS * ITER_PER_VU * AMOUNT_MINOR + 100000;

  // The wallet is opened through the FIRST target; subsequent writes spread.
  const opened = openWallet(cfg.targets[0], internal, player, minorToDecimal(seedMinor), `dist-open-${run}`);
  if (!opened.body.id) {
    fail(`setup could not open wallet: ${opened.status} ${JSON.stringify(opened.body)}`);
  }

  return { run, walletId: opened.body.id, playerId: player, seedMinor };
}

export default function (data) {
  const vu = exec.vu.idInTest;
  const it = exec.vu.iterationInInstance;
  const target = pickTarget(vu, it);
  const kind = it % 2 === 0 ? 'BET' : 'WIN';
  const { ext, key } = opIdentity(data.run, `dist${vu}`, it);

  const res = submit(
    target,
    providerToken(),
    {
      externalTransactionId: ext,
      playerId: data.playerId,
      walletId: data.walletId,
      roundId: `dist-round-${vu}`,
      kind,
      amount: cfg.amount,
    },
    key
  );
  moneyLatency.add(res.timings.duration);

  if (res.status === 200 || res.status === 201) {
    if (res.body.status === 'PROCESSED') appliedCount.add(1);
  } else if (res.status === 409) {
    conflictCount.add(1);
  }

  check(res, { 'submit is 2xx': (r) => r.status === 200 || r.status === 201 });
}

export function teardown(data) {
  const internal = internalToken();
  // Read through EVERY target: a balance that differs between replicas would
  // mean a stale cache or a read path that bypasses the row lock.
  const balances = [];
  for (const target of cfg.targets) {
    const w = getWallet(target, internal, data.walletId);
    if (w.status !== 200) {
      fail(`teardown: GET via ${target} returned ${w.status}`);
    }
    balances.push({ target, amount: w.body.balance.amount, version: w.body.version });
  }

  const first = balances[0];
  for (const b of balances) {
    if (b.amount !== first.amount || b.version !== first.version) {
      fail(
        `REPLICA DIVERGENCE: ${first.target} reports ${first.amount}/v${first.version} but ` +
          `${b.target} reports ${b.amount}/v${b.version}`
      );
    }
  }

  const perVU = ITER_PER_VU;
  const debits = Math.ceil(perVU / 2) * VUS;
  const credits = Math.floor(perVU / 2) * VUS;
  const expectedMinor = data.seedMinor - debits * AMOUNT_MINOR + credits * AMOUNT_MINOR;
  const actualMinor = decimalToMinor(first.amount);

  if (actualMinor !== expectedMinor) {
    fail(
      `CROSS-PROCESS LOCK FAILURE: balance is ${first.amount}, expected ${minorToDecimal(expectedMinor)} ` +
        `across ${cfg.targets.length} instances — the lock did not hold between processes`
    );
  }

  const rec = reconcile(cfg.targets[0], internal, data.walletId);
  if (rec.body.consistent !== true) {
    fail(`reconciliation disagrees: ${JSON.stringify(rec.body)}`);
  }

  console.log(
    `distributed OK — ${cfg.targets.length} instances agree on ${first.amount} (v${first.version}), ` +
      `${rec.body.checkedEntries} ledger entries, reconciliation consistent`
  );
}

function playerFor(run) {
  const tail = run.slice(-12).padStart(12, '0');
  return `${run.slice(-8).padStart(8, '0')}-4444-7444-8444-${tail}`;
}

export function handleSummary(data) {
  return {
    stdout: textSummary(data, { title: 'distributed' }),
  };
}
