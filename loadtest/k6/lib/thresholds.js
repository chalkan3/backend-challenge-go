// Shared k6 thresholds + summary.
//
// Design rule: a threshold must be able to FAIL. A check that always passes is
// documentation, not a test.
//
// But an envelope is only meaningful *relative to the machine that produced the
// number*, and this file used to ignore that. The percentile limits below were
// measured on a development machine (Podman, compose, one combined instance) and
// then asserted unchanged on a shared GitHub runner, which is a different
// machine by a factor that is not small. Observed on the same scenario and the
// same VU count:
//
//     hot-wallet p95   development machine   76ms   -> envelope p95<200 passes
//     hot-wallet p95   GitHub runner        297ms   -> envelope p95<200 FAILS
//
// That is ~3.9x, and it failed on every push — not because the service got
// slower, but because the envelope was calibrated somewhere else. A red build
// that no code change can fix teaches people to ignore red builds.
//
// So the two kinds of threshold are now separated by what they actually assert:
//
//   • CORRECTNESS is absolute and identical everywhere. `checks: rate==1` and
//     `http_req_failed: rate==0` do not move with hardware: a single 5xx, a
//     single lost update or a single duplicated debit fails the run on any
//     machine, fast or slow. This is the real gate.
//
//   • LATENCY is a budget, and which budget applies is chosen by the environment
//     (WALLET_LATENCY_PROFILE, set by the Makefile), not assumed. `local` uses
//     the measured numbers and stays tight enough to catch a regression while
//     developing; `ci` uses a wider envelope whose job is to catch an order-of-
//     magnitude regression on a runner whose absolute latency says little.
//
// The numbers are per environment on purpose. Pretending one number is correct
// on both machines is what produced the false failure this replaces.
import { Trend } from 'k6/metrics';

// Custom trends so the summary separates the money path from the read paths.
export const moneyLatency = new Trend('wallet_money_latency', true);
export const readLatency = new Trend('wallet_read_latency', true);

// A failing check is always fatal for the run. Absolute, on every machine.
export const correctness = {
  checks: ['rate==1'],
  http_req_failed: ['rate==0'],
};

// ---------------------------------------------------------------------------
// Which latency profile applies.
//
// Set by WALLET_LATENCY_PROFILE. The default is `local` because that is the
// conservative choice when a developer runs a scenario by hand: a tight envelope
// fails loudly, and a false failure during development is cheap to investigate.
// CI opts into `ci` explicitly, in the Makefile, so the widening is visible in
// review rather than hidden here.
const PROFILE = __ENV.WALLET_LATENCY_PROFILE || 'local';

if (PROFILE !== 'local' && PROFILE !== 'ci') {
  throw new Error(
    `WALLET_LATENCY_PROFILE=${PROFILE} is not a known profile; expected 'local' or 'ci'. ` +
      'Refusing to guess an envelope: a typo must not silently widen the gate.'
  );
}

// ---------------------------------------------------------------------------
// Latency envelopes, per traffic shape, per environment.
//
// They are NOT the same number across shapes, and that is the point. The
// hot-wallet, distributed and soak scenarios deliberately serialize every write
// on ONE row, so their latency is a queueing delay that scales with VU count —
// measuring that against the mixed-traffic number would either be a permanent
// failure or a rubber stamp. The mixed scenario spreads over independent wallets
// and is the one that reflects steady-state production latency.
//
// `local` — measured on a development machine (Podman, compose, 1 combined
// instance), envelope = measured p95 + ~50% headroom:
//   hot-wallet   p50  70ms  p95  76ms   (48 VUs, one row)
//   distributed  p50 117ms  p95 310ms   (96 VUs over 3 instances, one row)
//   mixed        p50  11ms  p95  32ms   (48 VUs, 200 wallets)
//   smoke        p50  <1ms  p95   4ms   (1 VU)
//
// `ci` — a shared GitHub runner, measured ~3.9x slower on the same hot-wallet
// shape (p95 297ms at 48 VUs, 336 iterations). These envelopes are set well
// above that so ordinary runner variance does not fail the build, while a real
// order-of-magnitude regression still does. p99 is dropped where the tail on a
// shared runner is dominated by noise rather than by the service.
const envelopes = {
  local: {
    money: { wallet_money_latency: ['p(95)<750', 'p(99)<2000'], wallet_read_latency: ['p(95)<500'] },
    hotWallet: { wallet_money_latency: ['p(95)<200', 'p(99)<500'] },
    distributed: { wallet_money_latency: ['p(95)<600', 'p(99)<1200'] },
    mixed: { wallet_money_latency: ['p(95)<100', 'p(99)<300'], wallet_read_latency: ['p(95)<80'] },
    soak: { wallet_money_latency: ['p(95)<200'] },
  },
  ci: {
    // Measured p95 ~297ms locally-to-CI ratio applied with headroom. The read
    // envelope widens by more because cheap requests are proportionally hurt
    // most by a noisy neighbour on a shared runner.
    money: { wallet_money_latency: ['p(95)<2000', 'p(99)<6000'], wallet_read_latency: ['p(95)<1500'] },
    hotWallet: { wallet_money_latency: ['p(95)<2000'] },
    distributed: { wallet_money_latency: ['p(95)<4000'] },
    mixed: { wallet_money_latency: ['p(95)<1500'], wallet_read_latency: ['p(95)<1000'] },
    soak: { wallet_money_latency: ['p(95)<2000'] },
  },
};

const selected = envelopes[PROFILE];

// Default traffic shape: reuse across the idempotency scenario and anything
// that is not a deliberate single-row contention test.
export const moneyThresholds = selected.money;

// One row, every write serialized. The scenario's own arithmetic (checked in
// teardown) is what actually proves the lock works; this envelope only catches
// a latency blow-up.
export const hotWalletThresholds = selected.hotWallet;

// Same contract spread over several processes: adds network hops and more
// contenders, hence a wider envelope.
export const distributedThresholds = selected.distributed;

// Independent wallets: this is the number to watch for regressions in the
// steady state.
export const mixedThresholds = selected.mixed;

// A soak runs long enough that the tail dominates; keep the envelope, drop the
// aggressive p99.
export const soakThresholds = selected.soak;
