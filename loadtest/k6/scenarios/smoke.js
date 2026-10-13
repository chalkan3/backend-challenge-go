// Scenario 0 — smoke. The gate that runs on every CI run.
//
// Not a load test: one VU, a handful of iterations, but it walks the FULL
// contract end to end (auth, open, bet, win, replay, conflict, reconciliation)
// so that a broken contract fails fast before the expensive scenarios start.
// It is the k6 equivalent of `curl` in the README, with assertions.
import { check, fail } from 'k6';
import http from 'k6/http';
import { textSummary } from '../lib/summary.js';
import { cfg } from '../lib/config.js';
import { internalToken, providerToken } from '../lib/auth.js';
import {
  decimalToMinor,
  getLedger,
  getWallet,
  minorToDecimal,
  openWallet,
  reconcile,
  submit,
  uuid,
} from '../lib/wallet.js';
import { correctness } from '../lib/thresholds.js';

export const options = {
  scenarios: {
    smoke: { executor: 'per-vu-iterations', vus: 1, iterations: 3, maxDuration: '1m' },
  },
  thresholds: correctness,
  summaryTrendStats: ['avg', 'med', 'p(95)', 'max'],
};

export default function () {
  const internal = internalToken();
  const provider = providerToken();
  const run = `${Date.now()}-${__VU}`;
  const player = uuid();

  // 1. health endpoints are part of the contract.
  const live = http.get(`${cfg.api}/health/live`);
  const ready = http.get(`${cfg.api}/health/ready`);
  check(live, { 'health/live is 200': (r) => r.status === 200 });
  check(ready, { 'health/ready is 200': (r) => r.status === 200 });

  // 2. open with an Idempotency-Key and prove the replay path.
  const key = `smoke-open-${run}`;
  const opened = openWallet(cfg.api, internal, player, '100.00', key);
  if (opened.status !== 201 && opened.status !== 200) {
    fail(`smoke: open failed: ${opened.status} ${JSON.stringify(opened.body)}`);
  }
  const walletId = opened.body.id;
  if (!walletId) {
    fail(`smoke: open returned no wallet id: ${JSON.stringify(opened.body)}`);
  }
  check(opened.body, {
    'open returns a decimal string balance': (b) => /^\d+\.\d{2}$/.test(b.balance.amount),
    'open starts at version 1': (b) => b.version === 1,
  });

  const replayed = openWallet(cfg.api, internal, player, '100.00', key);
  check(replayed.body, {
    'same key replays the same wallet': (b) => b.id === walletId,
    'replay is flagged': (b) => b.idempotentReplay === true,
  });

  // 3. bet, then replay it, then conflict it.
  const ext = `smoke-bet-${run}`;
  const bet = submit(
    cfg.api,
    provider,
    { externalTransactionId: ext, playerId: player, walletId, roundId: `r-${run}`, kind: 'BET', amount: '25.00' },
    `provider-a:${ext}`
  );
  check(bet.body, {
    'bet is PROCESSED': (b) => b.status === 'PROCESSED',
    'bet debits 25.00': (b) => b.balance && b.balance.amount === '75.00',
  });

  const betReplay = submit(
    cfg.api,
    provider,
    { externalTransactionId: ext, playerId: player, walletId, roundId: `r-${run}`, kind: 'BET', amount: '25.00' },
    `provider-a:${ext}`
  );
  check(betReplay.body, {
    'bet replay is flagged and moves nothing': (b) => b.idempotentReplay === true && b.balance.amount === '75.00',
  });

  const conflict = submit(
    cfg.api,
    provider,
    { externalTransactionId: ext, playerId: player, walletId, roundId: `r-${run}`, kind: 'BET', amount: '30.00' },
    `provider-a:${ext}`
  );
  check(conflict, {
    'same key + different amount is 409': (r) => r.status === 409,
    '409 carries IDEMPOTENCY_KEY_CONFLICT': (r) => (r.body.error || {}).code === 'IDEMPOTENCY_KEY_CONFLICT',
  });

  // 4. win brings it back up.
  const winExt = `smoke-win-${run}`;
  const win = submit(
    cfg.api,
    provider,
    { externalTransactionId: winExt, playerId: player, walletId, roundId: `r-${run}`, kind: 'WIN', amount: '10.00' },
    `provider-a:${winExt}`
  );
  check(win.body, {
    'win is PROCESSED': (b) => b.status === 'PROCESSED',
    'win credits 10.00': (b) => b.balance && b.balance.amount === '85.00',
  });

  // 5. insufficient funds is a mapped refusal, not a crash.
  const overExt = `smoke-over-${run}`;
  const over = submit(
    cfg.api,
    provider,
    {
      externalTransactionId: overExt,
      playerId: player,
      walletId,
      roundId: `r-${run}`,
      kind: 'BET',
      amount: '1000000.00',
    },
    `provider-a:${overExt}`
  );
  check(over, {
    'overdraw is 422': (r) => r.status === 422,
  });

  // 6. the read paths and the reconciliation contract.
  const wallet = getWallet(cfg.api, internal, walletId);
  check(wallet.body, {
    'balance is 85.00 after bet+win': (b) => b.balance.amount === '85.00',
    'version advanced to 3': (b) => b.version === 3,
  });

  const ledger = getLedger(cfg.api, internal, walletId, 50);
  check(ledger.body, {
    'ledger has entries': (b) => Array.isArray(b.items) && b.items.length >= 1,
    'ledger entries carry a running balance': (b) =>
      b.items.every((e) => e.balanceAfter && e.balanceBefore && e.direction),
  });

  const rec = reconcile(cfg.api, internal, walletId);
  check(rec.body, {
    'reconciliation is consistent': (b) => b.consistent === true,
    'reconciliation difference is zero': (b) => b.difference.amount === '0.00',
  });
}

export function handleSummary(data) {
  return {
    stdout: textSummary(data, { title: 'smoke' }),
  };
}
