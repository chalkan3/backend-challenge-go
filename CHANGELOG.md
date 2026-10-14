# Changelog

## [0.2.0](https://github.com/chalkan3/backend-challenge-go/compare/walletd-v0.1.0...walletd-v0.2.0) (2026-10-14)


### Features

* **auth,sqs:** políticas de Keycloak e SQS, cada uma provada contra a stack viva ([4aba06b](https://github.com/chalkan3/backend-challenge-go/commit/4aba06bc7e4eaf0129ce1c0057c7bd9397e6c767))
* **build:** binário e imagem passam a saber a própria versão, e o GoReleaser publica releases ([c37aee8](https://github.com/chalkan3/backend-challenge-go/commit/c37aee89f4be3e7639faff65750fcb8013a1b91b))
* **ci:** release-please decide a versão, e o GoReleaser anexa os binários ([b4289d3](https://github.com/chalkan3/backend-challenge-go/commit/b4289d3da144cfd4a2141cdbda44c3419faf5e98))
* **deploy:** ambiente Kubernetes local (kind) como segunda forma de subir ([a4f0145](https://github.com/chalkan3/backend-challenge-go/commit/a4f0145961fd1817100c241c679a79a430a6c20b))
* **deploy:** um binário com três modos (WALLET_MODE) e topologia split ([3d74162](https://github.com/chalkan3/backend-challenge-go/commit/3d74162fcd9f9a9c5a9535b09c6ed678ebe094b6))
* **domain:** partidas dobradas como núcleo de domínio ([2ac453a](https://github.com/chalkan3/backend-challenge-go/commit/2ac453a323d253d273a80f3d3b3ebed80ca50c44))
* **infra:** as filas SQS como código, ao lado do init do LocalStack ([5ec38f6](https://github.com/chalkan3/backend-challenge-go/commit/5ec38f675f832516fc34242e5f349509dfa04517))
* **infra:** PgBouncer (transaction mode) e pgAudit, com prova em ambos ([8b556ea](https://github.com/chalkan3/backend-challenge-go/commit/8b556ea61d1ffe53f8da73430e65971aa8c46b0f))
* **ledger:** partidas dobradas plugadas ao SQL, com auditorias [#11](https://github.com/chalkan3/backend-challenge-go/issues/11)/[#11](https://github.com/chalkan3/backend-challenge-go/issues/11)b/[#11](https://github.com/chalkan3/backend-challenge-go/issues/11)c ([5b76729](https://github.com/chalkan3/backend-challenge-go/commit/5b76729021b1840326a56bbfff9f1bc47b252fda))
* **observability:** stack de métricas/traces/logs e dashboards como código ([363eb69](https://github.com/chalkan3/backend-challenge-go/commit/363eb699c159b49029547b83b7f6a1124d46861e))
* **otel:** tracing distribuído com OpenTelemetry (opt-in) ([3fb5bbc](https://github.com/chalkan3/backend-challenge-go/commit/3fb5bbc564edf4a5c4769ab3eae0a7a65e8db558))
* **reconcile:** varredura contínua de integridade + métricas de sweep ([3b4555d](https://github.com/chalkan3/backend-challenge-go/commit/3b4555dcde93170a889573e073ff85364f1237eb))
* **tracing:** continuidade do trace HTTP→outbox→SQS→consumer ([6af3449](https://github.com/chalkan3/backend-challenge-go/commit/6af3449eb88ce306bc5222567dd8ff0fb91aa9fb))
* wallet service — Go + go-kit + Uber Fx ([5e5466c](https://github.com/chalkan3/backend-challenge-go/commit/5e5466c685d8a618a6cf6621cbdd0785420bf33c))
* **wallet:** idempotência persistente na abertura de carteira ([fbddbca](https://github.com/chalkan3/backend-challenge-go/commit/fbddbcaaa62c0840595fdae041b8f3f52442522b))


### Bug Fixes

* **auth:** JWKS recarrega de verdade e sem data race ([42950a3](https://github.com/chalkan3/backend-challenge-go/commit/42950a3ac6089e6d498a6eebedc0a1e64c32d2dc))
* **ci:** o gate de cobertura mede o mesmo codigo toda vez ([3e51963](https://github.com/chalkan3/backend-challenge-go/commit/3e51963d9282f38cd211bbebdcb458cc633739e0))
* **domain:** ordenação estável no journal; delta do jogador independente da ordem das pernas ([2e28264](https://github.com/chalkan3/backend-challenge-go/commit/2e28264975e10b3732cb8ff9d1a4030064c96856))
* **load:** load-verify para de adivinhar o nome do container do Postgres ([152e8f3](https://github.com/chalkan3/backend-challenge-go/commit/152e8f354ec9875f5cae132f785e555ef39565ca))
* **load:** o envelope de latencia pertence a maquina que o mediu ([b933950](https://github.com/chalkan3/backend-challenge-go/commit/b933950adf7bcd76260ac0a9addb2be8382dcdb9))
* **release,ci:** a release que publica de verdade, e provas de que a versão chega ao binário ([9180225](https://github.com/chalkan3/backend-challenge-go/commit/9180225da30e6afb1007d8db9846a24fa44d3e65))
* **test:** o outbox nao pertence ao teste quando um worker real esta vivo ([1eabc49](https://github.com/chalkan3/backend-challenge-go/commit/1eabc49ec63bbbaf950d7ff3b9043e4d57498267))
* **test:** o reference worker tambem drena a tabela do teste ([ed7bf3b](https://github.com/chalkan3/backend-challenge-go/commit/ed7bf3b35e7d48a64cc4a4f9b11d908b37e65ce4))


### Refactors

* **domain,service:** Specification e Strategy no lugar de cadeias de if ([669334b](https://github.com/chalkan3/backend-challenge-go/commit/669334b7e455bfbe66505cb76875abbe92e0d2fc))
* idiomatic Go + go-kit architecture — package layout, design patterns, testify ([5af2b9f](https://github.com/chalkan3/backend-challenge-go/commit/5af2b9ff643bf8c035d2de23a69caebcdaa04310))
