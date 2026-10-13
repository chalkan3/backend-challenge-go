// Shared configuration for every wallet load scenario.
//
// Everything is overridable by environment variable so the same scripts run
// against compose (single app on :8081), compose --profile multi (three
// replicas on :8081/:8082/:8083) and the kind cluster (same host ports), without
// editing a file.

const env = (key, fallback) =>
  __ENV[key] === undefined || __ENV[key] === '' ? fallback : __ENV[key];

const asList = (raw) =>
  raw
    .split(',')
    .map((s) => s.trim())
    .filter((s) => s.length > 0);

const single = env('API', 'http://localhost:8081');
const listed = asList(env('APIS', ''));

export const cfg = {
  // Keycloak
  keycloak: env('KEYCLOAK', 'http://localhost:8080'),
  realm: env('REALM', 'wallet'),

  // Identities from deploy/keycloak/realm-wallet.json
  internal: { clientId: 'wallet-internal', secret: env('INTERNAL_SECRET', 'internal-secret') },
  providerA: { clientId: 'provider-a', secret: env('PROVIDER_A_SECRET', 'provider-a-secret') },

  // API targets. `API` is one base URL; `APIS` is a comma-separated list used
  // by the distributed scenario to spread load across replicas.
  api: single,
  targets: listed.length > 0 ? listed : [single],

  currency: env('CURRENCY', 'BRL'),
  amount: env('AMOUNT', '1.00'),
  gameId: env('GAME_ID', 'load-test'),

  // Fraction (0..1) of adversarial rows in the mixed scenario. Kept low so they
  // never dominate and hide the happy-path latency.
  badFraction: Number(env('BAD_FRACTION', '0.02')),

  // An amount guaranteed to overdraw a freshly opened wallet.
  overdrawAmount: env('OVERDRAW_AMOUNT', '100000000.00'),
};

// Round-robin helper shared by the distributed scenario.
export function pickTarget(vu, iter) {
  return cfg.targets[(vu + iter) % cfg.targets.length];
}
