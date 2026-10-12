.PHONY: build fmt vet test test-race test-integration e2e up up-multi down logs migrate-up migrate-down migrate-status

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
#   make e2e ARGS="-run Journey"
#   make e2e MULTI=1          # inclui as instâncias extras (app-2/app-3)
#   make e2e KEEP=1           # deixa a stack no ar ao final
e2e:
	./scripts/e2e.sh $(ARGS)

up:
	docker compose up --build -d

up-multi:
	docker compose --profile multi up --build -d

down:
	docker compose down -v

logs:
	docker compose logs -f app

# Migrations manuais (o binário também aplica up no start; down é manual e
# auditável, um passo por execução).
migrate-up:
	go run ./cmd/walletd migrate up

migrate-down:
	go run ./cmd/walletd migrate down

migrate-status:
	go run ./cmd/walletd migrate status
