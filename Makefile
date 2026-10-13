.PHONY: build fmt vet test test-race test-integration e2e up up-multi up-split down logs migrate-up migrate-down migrate-status kind-check kind-up kind-image kind-deploy kind-split kind-down kind-argocd kind-argocd-seed kind-status helm-lint engine-check preflight

# Engine/compose detection lives in scripts/engine.sh (podman vs docker). We ask
# it once per recipe via $(shell ...) so `make up`, `make logs`, ... use exactly
# the same implementation the scripts do — instead of assuming `docker compose`,
# which breaks on podman-only hosts.
COMPOSE := $(shell . ./scripts/engine.sh >/dev/null 2>&1 && echo "$$COMPOSE")

# Fails with a clear message if no compose implementation is available.
engine-check:
	@. ./scripts/engine.sh >/dev/null && echo "compose: $$COMPOSE (engine: $$CONTAINER_ENGINE)"

# Fails early when the compose host ports are already taken (e.g. by a kind
# cluster); without it `up` silently leaves containers stuck in "Created".
preflight:
	@./scripts/port-check.sh

build:
	go build ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

test:
	go test ./...

test-race:
	go test -race ./...

# Testes de integração: exige o ambiente do compose no ar (Postgres, Keycloak,
# LocalStack com as filas provisionadas).
test-integration:
	go test -tags=integration ./test/integration/... -v -timeout 10m

# Validação end-to-end em um comando: sobe a stack, espera os healthchecks de
# verdade, roda a suíte (incluindo os testes de jornada) e derruba tudo.
#   make e2e                  # stack completa + suíte
#   make e2e SPLIT=1          # api + workers como processos separados
#   make e2e ARGS="-run Journey"
#   make e2e MULTI=1          # inclui as instâncias extras (app-2/app-3)
#   make e2e KEEP=1           # deixa a stack no ar ao final
e2e:
	SPLIT=$(SPLIT) MULTI=$(MULTI) KEEP=$(KEEP) ./scripts/e2e.sh $(ARGS)

up: preflight
	$(COMPOSE) up --build -d

up-multi: preflight
	$(COMPOSE) --profile multi up --build -d

# Two-role topology: one image, two processes (WALLET_MODE=api / workers).
# api :8084 owns the whole pool for request handling; workers :8085 drains the
# queue. Scale each side on its own dimension: --scale workers=4.
up-split: preflight
	$(COMPOSE) --profile split up --build -d

down:
	$(COMPOSE) down -v

logs:
	$(COMPOSE) logs -f app

# Migrations manuais (o binário também aplica up no start; down é manual e
# auditável, um passo por execução).
migrate-up:
	go run ./cmd/walletd migrate up

migrate-down:
	go run ./cmd/walletd migrate down

migrate-status:
	go run ./cmd/walletd migrate status

# ===========================================================================
# Kubernetes local (kind)
# ===========================================================================
# A segunda forma de subir o ambiente: em vez de containers soltos via
# compose, tudo é reconciliado por Kubernetes a partir de um Helm chart.
#
#   make kind-deploy            # cluster + imagem + chart, tudo em um passo
#   make kind-deploy OBSERVABILITY=1 SPLIT=1
#
# O passo de imagem é local de ponta a ponta: `kind load docker-image` copia o
# tarball direto para o containerd do node — não há registry neste caminho.
#
# Sobre o pré-requisito: kind é checado, não presumido (kind-check). Se faltar,
# o alvo falha listando o que instalar, em vez de estourar no meio do deploy.

# Verifica kind/helm/kubectl e mostra qual engine será usada.
kind-check:
	./scripts/kind-check.sh

# Sobe apenas o cluster (idempotente: reusa se já existir).
kind-up:
	CLUSTER_NAME=$(CLUSTER_NAME) RECREATE=$(RECREATE) ./scripts/kind-up.sh

# Builda a imagem local e a carrega no node do cluster.
kind-image:
	./scripts/kind-image.sh

# Instala/atualiza o release no cluster (cluster e imagem precisam existir).
kind-deploy:
	SPLIT=$(SPLIT) OBSERVABILITY=$(OBSERVABILITY) RECREATE=$(RECREATE) \
		VALUES="$(VALUES)" SET="$(SET)" ./scripts/kind-deploy.sh

# Mesma coisa, na topologia api+workers (um binário, dois papéis).
kind-split:
	SPLIT=1 OBSERVABILITY=$(OBSERVABILITY) ./scripts/kind-deploy.sh

# Caminho GitOps opcional: ArgoCD + git host in-cluster e o Application.
kind-argocd: kind-deploy
	./scripts/kind-argocd.sh

# Re-semeia a pasta local no git in-cluster (é o que dispara um sync novo).
kind-argocd-seed:
	./scripts/kind-argocd-seed.sh $(or $(CLUSTER_NAME),wallet)

# Estado do release e dos workloads.
kind-status:
	helm -n $(or $(NAMESPACE),default) status $(or $(RELEASE_NAME),wallet)
	kubectl -n $(or $(NAMESPACE),default) get pods,svc,deploy

# Apaga o cluster inteiro (e com ele os volumes).
kind-down:
	kind delete cluster --name $(or $(CLUSTER_NAME),wallet)

# Renderiza e valida o chart sem tocar em cluster nenhum.
helm-lint:
	helm lint deploy/kubernetes/charts/wallet --set observability.enabled=true
	helm template wallet deploy/kubernetes/charts/wallet >/dev/null
	helm template wallet deploy/kubernetes/charts/wallet --set split.enabled=true >/dev/null
	helm template wallet deploy/kubernetes/charts/wallet \
		--set split.enabled=true --set observability.enabled=true --set observability.otel.enabled=true >/dev/null
	@echo "chart renders in all topologies"
