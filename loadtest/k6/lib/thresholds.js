// Shared k6 thresholds + summary.
//
// Design rule: a threshold must be able to FAIL. Percentile limits are set from
// measured numbers (see loadtest/README.md §Calibração), not guessed — a check
// that always passes is documentation, not a test.
//
// The one threshold that is absolute rather than statistical is `checks`:
// correctness has no acceptable error budget here. A single 5xx, a single lost
// update or a single duplicated debit is a failure regardless of how fast the
// service was.
import { Trend } from 'k6/metrics';

// Custom trends so the summary separates the money path from the read paths.
export const moneyLatency = new Trend('wallet_money_latency', true);
export const readLatency = new Trend('wallet_read_latency', true);

// A failing check is always fatal for the run.
export const correctness = {
  checks: ['rate==1'],
  http_req_failed: ['rate==0'],
};

// ---------------------------------------------------------------------------
// Latency envelopes, per traffic shape.
//
// They are NOT the same number, and that is the point. The hot-wallet and
// distributed scenarios deliberately serialize every write on ONE row, so their
// latency is a queueing delay that scales with VU count — measuring it against
// the mixed-traffic number would either be a permanent failure or a rubber
// stamp. The mixed scenario spreads over independent wallets and is the one that
// reflects steady-state production latency.
//
// Measured on this machine (Podman, compose, 1 combined instance):
//   hot-wallet   p50  70ms  p95  76ms   (48 VUs, one row)
//   distributed  p50 117ms  p95 310ms   (96 VUs over 3 instances, one row)
//   mixed        p50  11ms  p95  32ms   (48 VUs, 200 wallets)
//   smoke        p50  <1ms  p95   4ms   (1 VU)
//
// Envelope = measured p95 + ~50% headroom, so a real regression trips it while
// normal variance does not.
export const moneyThresholds = {
  wallet_money_latency: ['p(95)<750', 'p(99)<2000'],
  wallet_read_latency: ['p(95)<500'],
};

// One row, many writers: latency is queueing, so the bound is generous and the
// invariant (checked in teardown) is what actually proves the lock works.
export const hotWalletThresholds = {
  wallet_money_latency: ['p(95)<200', 'p(99)<500'],
};

// Same contract spread over several processes: adds network hops and more
// contenders, hence a wider envelope.
export const distributedThresholds = {
  wallet_money_latency: ['p(95)<600', 'p(99)<1200'],
};

// Independent wallets: this is the number to watch for regressions in the
// steady state.
export const mixedThresholds = {
  wallet_money_latency: ['p(95)<100', 'p(99)<300'],
  wallet_read_latency: ['p(95)<80'],
};

// A soak runs long enough that the tail dominates; keep the envelope, drop the
// aggressive p99.
export const soakThresholds = {
  wallet_money_latency: ['p(95)<200'],
};
