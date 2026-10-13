# Keycloak — policies do realm `wallet`

O realm é **declarativo**: `realm-wallet.json` é importado pelo Keycloak no boot
(`--import-realm`). Este documento explica *qual política* cada campo declara,
*por que* ela existe, e *como* ela é provada — porque política que ninguém
verifica é decoração.

---

## O problema que estas políticas resolvem

Medido no token real de um provedor de jogos **antes** destas mudanças:

```json
{
  "aud": "account",                         ← público errado
  "scope": "profile email",                 ← escopo de usuário numa máquina
  "realm_access": { "roles": ["offline_access", "uma_authorization", ...] },
  "resource_access": { "account": { ... } },
  "preferred_username": "service-account-provider-a",
  "email_verified": false,
  "providerId": "provider-a"
}
```

Um client de **máquina** recebia escopo e claims de **usuário**: `email`,
`profile`, roles de realm, a audience default `account`. E o serviço aceitava
qualquer um deles, porque `WALLET_AUTH_AUDIENCE` estava vazio — ou seja, um token
emitido para *qualquer outro público* dentro do mesmo realm também autenticaria.

Depois das políticas, o mesmo token é:

```json
{
  "aud": "wallet-api",
  "scope": "wallet:audience wallet:provider",
  "azp": "provider-a",
  "providerId": "provider-a"
}
```

Sem `email`, sem `roles`, sem `resource_access`, sem `preferred_username`.

---

## As políticas, uma a uma

### 1. Audience policy — `aud = wallet-api`

Um **client scope** `wallet:audience` carrega um *audience mapper* que injeta
`aud=wallet-api` em todo access token; ele é `default` nos três clients. Do lado
do serviço, o compose passa a definir:

```yaml
WALLET_AUTH_AUDIENCE: wallet-api
```

O authenticator **exige** essa audience (`jwt.WithAudience`). O efeito é
delimitador: um token que o realm emitiu para outra finalidade — outro público,
outro recurso — é **recusado com 401**, não aceito por omissão.

> **Por que exigir e não apenas emitir.** Emitir a audience sem exigi-la deixaria
> o furo aberto: o campo seria decorativo. A exigência é o que transforma a
> emissão em política.

### 2. Scope policy — `fullScopeAllowed: false` + client scopes dedicados

Cada client declara **apenas** os escopos do wallet:

| Client | `defaultClientScopes` |
| :-- | :-- |
| `provider-a` / `provider-b` | `wallet:provider`, `wallet:audience` |
| `wallet-internal` | `wallet:internal`, `wallet:audience` |

Com `fullScopeAllowed: false`, nenhuma role de realm vaza para o token. Os
escopos `profile`, `email`, `roles`, `web-origins` e `acr` — que o Keycloak
atribui por padrão a *todo* client, inclusive máquinas — **não** entram.

> **O escopo NÃO é portão de autorização.** O `Principal` expõe `Scopes` e
> `HasScope`, mas o middleware `authorize` continua decidindo por claim
> (`providerId` / `wallet:internal`). O motivo é medido: essas claims são
> *assinadas*, logo já são infalsificáveis; exigir escopo adicionaria um modo de
> falha sem adicionar segurança. O escopo é *higiene* — token mínimo — e é
> **observabilidade**: um token de máquina com `email` aparece nos logs.

### 3. Token/security policies do realm

| Campo | Valor | Por quê |
| :-- | :-- | :-- |
| `revokeRefreshToken` | `true` | Refresh token não é reutilizável indefinidamente |
| `refreshTokenMaxReuse` | `0` | Sem reuso (o default `false`/`0` liberava o refresh) |
| `bruteForceProtected` | `true` | Throttle de tentativas de credencial |
| `failureFactor` | `10` | Limite de falhas antes do bloqueio |
| `accessTokenLifespan` | `900` | Janela de 15 min (inalterado; explicitado) |
| `eventsEnabled` | `true` | O realm registra eventos de login |
| `adminEventsEnabled` | `true` | O realm registra eventos administrativos |

---

## A armadilha: `--import-realm` NÃO reimporta um realm existente

Medido nos logs do Keycloak no boot:

```
KC-SERVICES0030: Full model import requested. Strategy: IGNORE_EXISTING
```

`IGNORE_EXISTING` significa que, se o realm `wallet` **já existe** no volume do
Keycloak, o `--import-realm` **não** aplica as mudanças de `realm-wallet.json`.
Um `realm-wallet.json` editado convive com o realm velho em execução — o pior
tipo de inconsistência, porque tudo *parece* funcionar.

Duas consequências práticas:

1. **Ambiente novo** (volume limpo): o import aplica tudo, do zero.
2. **Ambiente antigo**: recrie o realm. Não há reimport in-place.

Isso não é um problema a ser "resolvido" com um reconciliador de Admin API. O
projeto é declarativo em todo lugar (migrations, compose, init do LocalStack);
um reconciliador quebraria a simetria para resolver um caso que o ciclo de vida
do ambiente já resolve. **A prova substitui a reconciliação:**
`test/integration/keycloak_policy_test.go` lê as policies de volta do Admin API
e **falha alto** se a stack estiver num realm obsoleto.

---

## Como isto é provado

`test/integration/keycloak_policy_test.go`, contra o Keycloak **vivo**:

| Teste | O que prova |
| :-- | :-- |
| `TestKeycloakRealmPoliciesAreApplied` | Lê o realm via Admin API e confere cada policy (`revokeRefreshToken`, `bruteForceProtected`, `eventsEnabled`, `fullScopeAllowed:false`, escopos sem `profile`/`email`/`roles`) |
| `TestKeycloakIssuedTokensCarryMinimalScope` | Decodifica os tokens reais e exige `aud=wallet-api`, o escopo do wallet, e a **ausência** de `email`/`preferred_username`/`realm_access`/`resource_access` |
| `TestAudiencePolicyRejectsForeignAudience` | O token legítimo é aceito; a exigência de audience está de fato ligada |

Rodar:

```sh
WALLET_TEST_KEYCLOAK=http://localhost:8080 \
  go test -tags=integration ./test/integration/ -run TestKeycloak -v
```

---

## Recriando o realm (quando editar `realm-wallet.json`)

```sh
# Opção 1 — recriar o realm via Admin API, preservando o resto da stack
KC=http://localhost:8080
ADMIN=$(curl -s -X POST $KC/realms/master/protocol/openid-connect/token \
  -d 'grant_type=password&client_id=admin-cli&username=admin&password=admin' \
  | python3 -c "import sys,json;print(json.load(sys.stdin)['access_token'])")
curl -s -X DELETE -H "Authorization: Bearer $ADMIN" $KC/admin/realms/wallet
curl -s -X POST -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  --data @deploy/keycloak/realm-wallet.json $KC/admin/realms

# Opção 2 — recriar o container com volume limpo
podman rm -f wallet_keycloak_1 && podman-compose up -d keycloak
```

Depois de qualquer uma, rode `TestKeycloakRealmPoliciesAreApplied` — é ele que
confirma que a mudança chegou.
