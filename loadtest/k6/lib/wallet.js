// Wallet API helpers.
//
// Each helper returns the parsed body together with the status, so scenarios can
// assert on either. Responses are parsed with `res.json()`; a helper never
// throws on a business status (409/422 are legitimate outcomes under load and
// are counted as such), it only throws on a transport failure.
import { check } from 'k6';
import exec from 'k6/execution';
import http from 'k6/http';
import { cfg } from './config.js';

const JSON_HEADERS = { 'Content-Type': 'application/json' };

// Every business endpoint can legitimately answer with a documented client-error
// code (404/409/422). In k6 v2 the expected-status list is a PER-REQUEST
// callback, not an option, so it has to be attached to each call. Without it
// `http_req_failed` counts a correct refusal as a transport failure and the
// threshold becomes meaningless.
//
// 5xx are deliberately NOT listed: those must remain failures.
const OK_OR_REFUSED = http.expectedStatuses({ min: 200, max: 202 }, 400, 401, 403, 404, 409, 422);
const OK_OR_404 = http.expectedStatuses({ min: 200, max: 202 }, 404);

// RFC4122 v4-shaped UUID. k6 has no crypto UUID source, and the service parses
// playerId with uuid.Parse — so a readable string like "player-1" is rejected as
// INVALID_TRANSACTION. Version and variant nibbles are fixed to 4/8 so the value
// is well-formed; the rest is random, which makes collisions across runs and VUs
// vanishingly unlikely.
export function uuid() {
  const hex = [];
  for (let i = 0; i < 16; i++) {
    hex.push((Math.floor(Math.random() * 256)).toString(16).padStart(2, '0'));
  }
  // version 4
  hex[6] = '4' + hex[6].slice(1);
  // variant 10xx
  const variantNibble = ((parseInt(hex[8][0], 16) & 0x3) | 0x8).toString(16);
  hex[8] = variantNibble + hex[8].slice(1);
  const s = hex.join('');
  return `${s.slice(0, 8)}-${s.slice(8, 12)}-${s.slice(12, 16)}-${s.slice(16, 20)}-${s.slice(20)}`;
}

let counter = 0;
export function uniq(prefix, run) {
  counter += 1;
  const vu = exec.vu && exec.vu.idInTest !== undefined ? exec.vu.idInTest : 0;
  return `${prefix}-${run}-${vu}-${counter}`;
}

function parse(res) {
  try {
    return res.json();
  } catch (e) {
    return null;
  }
}

// POST /wallets — opens a wallet. `Idempotency-Key` is optional in the API but
// every scenario sends one: without it, two VUs racing on the same
// (playerId, currency) would get 409 WALLET_ALREADY_EXISTS instead of a replay,
// and the race would be attributed to the wrong layer.
export function openWallet(base, token, playerId, initialBalance, idemKey) {
  const res = http.post(
    `${base}/wallets`,
    JSON.stringify({ playerId, initialBalance: { amount: initialBalance, currency: cfg.currency } }),
    {
      headers: Object.assign({ Authorization: `Bearer ${token}` }, JSON_HEADERS, idemKey ? { 'Idempotency-Key': idemKey } : {}),
      tags: { name: 'openWallet' },
      responseCallback: OK_OR_REFUSED,
    }
  );
  const body = parse(res);
  // A wallet open can legitimately answer 201 (created), 200 (idempotent
  // replay) or 409 (another wallet already exists for this player+currency).
  // Only a 5xx or an unexpected 4xx is a real failure.
  check(res, {
    'openWallet: created, replayed or refused': (r) =>
      r.status === 201 || r.status === 200 || r.status === 409,
  });
  return { status: res.status, body: body || {}, timings: res.timings };
}

// POST /wagering/transactions — the money path.
export function submit(base, token, req, idemKey) {
  const payload = {
    providerId: req.providerId || 'provider-a',
    externalTransactionId: req.externalTransactionId,
    playerId: req.playerId,
    walletId: req.walletId,
    roundId: req.roundId,
    gameId: req.gameId || cfg.gameId,
    kind: req.kind,
    money: { amount: req.amount, currency: cfg.currency },
  };
  if (req.referenceExternalTransactionId) {
    payload.referenceExternalTransactionId = req.referenceExternalTransactionId;
  }

  const res = http.post(`${base}/wagering/transactions`, JSON.stringify(payload), {
    headers: Object.assign(
      { Authorization: `Bearer ${token}` },
      JSON_HEADERS,
      idemKey ? { 'Idempotency-Key': idemKey } : {}
    ),
    tags: { name: 'submit' },
    responseCallback: OK_OR_REFUSED,
  });
  return { status: res.status, body: parse(res) || {}, timings: res.timings };
}

// Helper that builds the canonical (externalTransactionId, idempotencyKey) pair
// for one logical operation, so a replay reuses the EXACT same tuple.
export function opIdentity(run, vuMark, seq) {
  const ext = `lt-${run}-${vuMark}-${seq}`;
  return { ext, key: `provider-a:${ext}` };
}

export function getWallet(base, token, walletId) {
  const res = http.get(`${base}/wallets/${walletId}`, {
    headers: { Authorization: `Bearer ${token}` },
    tags: { name: 'getWallet' },
    responseCallback: OK_OR_404,
  });
  return { status: res.status, body: parse(res) || {}, timings: res.timings };
}

export function getLedger(base, token, walletId, limit) {
  const res = http.get(`${base}/wallets/${walletId}/ledger?limit=${limit || 100}`, {
    headers: { Authorization: `Bearer ${token}` },
    tags: { name: 'getLedger' },
    responseCallback: OK_OR_404,
  });
  return { status: res.status, body: parse(res) || {}, timings: res.timings };
}

export function reconcile(base, token, walletId) {
  const res = http.post(`${base}/wallets/${walletId}/reconciliation`, null, {
    headers: { Authorization: `Bearer ${token}` },
    tags: { name: 'reconcile' },
    responseCallback: OK_OR_404,
  });
  return { status: res.status, body: parse(res) || {}, timings: res.timings };
}

// Money as a decimal string (never a float). This mirrors the wire contract.
export function minorToDecimal(minor) {
  const neg = minor < 0;
  const abs = Math.abs(minor);
  const s = `${Math.floor(abs / 100)}.${String(abs % 100).padStart(2, '0')}`;
  return neg ? `-${s}` : s;
}

export function decimalToMinor(str) {
  const m = /^(-?)(\d+)\.(\d{2})$/.exec(str);
  if (!m) {
    throw new Error(`not a decimal string: ${str}`);
  }
  const minor = Number(m[2]) * 100 + Number(m[3]);
  return m[1] === '-' ? -minor : minor;
}
