// OAuth2 client_credentials against Keycloak, cached per VU.
//
// The token endpoint is a real dependency of every business request, so the
// scenarios authenticate the way a provider does — no bypass headers, no
// disabled auth. Tokens are cached until shortly before `expires_in`, so a long
// soak does not mint one token per request.
import http from 'k6/http';
import { cfg } from './config.js';

const cache = {}; // keyed by clientId, per VU

export function tokenFor(clientId, secret) {
  const now = Date.now();
  const hit = cache[clientId];
  if (hit && hit.expiresAt > now + 5000) {
    return hit.token;
  }

  const res = http.post(
    `${cfg.keycloak}/realms/${cfg.realm}/protocol/openid-connect/token`,
    { grant_type: 'client_credentials', client_id: clientId, client_secret: secret },
    { tags: { name: 'token' } }
  );

  if (res.status !== 200) {
    throw new Error(`token for ${clientId} failed: ${res.status} ${res.body}`);
  }

  const body = res.json();
  if (!body.access_token) {
    throw new Error(`token for ${clientId} has no access_token: ${res.body}`);
  }

  const ttl = Number(body.expires_in || 60) * 1000;
  cache[clientId] = { token: body.access_token, expiresAt: now + ttl };
  return body.access_token;
}

export const internalToken = () => tokenFor(cfg.internal.clientId, cfg.internal.secret);
export const providerToken = () => tokenFor(cfg.providerA.clientId, cfg.providerA.secret);

