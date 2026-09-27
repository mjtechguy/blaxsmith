.PHONY: test check build example web-check proto-check e2e api-reference

test:
	go test ./...

# Disposable PostgreSQL schemas, real HTTPS app, browser UI and MCP subprocess.
# Set BLAXSMITH_TEST_DATABASE_URL; Chrome is the default browser channel.
e2e:
	test -n "$$BLAXSMITH_TEST_DATABASE_URL"
	cd frontend && npm run build
	BLAXSMITH_E2E_BROWSER=1 go test ./cmd/blaxsmith -run 'TestServeAppHTTPSPostgres|TestAnvilExecutionCorrectionPostgres' -count=1 -v

check: test
	go vet ./...

build:
	go build -o bin/blaxsmith ./cmd/blaxsmith

example:
	go run ./cmd/blaxsmith check --recipe examples/anvil/recipe.json --scope .

web-check:
	cd frontend && npm run check:ui
	cd frontend && npm ci && npm run build
	cd frontend && npm run test:auth
	cd frontend && npm run test:workflow

proto-check:
	cd frontend && npm ci
	frontend/node_modules/.bin/buf lint
	frontend/node_modules/.bin/buf generate
	git diff --exit-code -- gen/go frontend/src/gen
	test -z "$$(git ls-files --others --exclude-standard -- gen/go frontend/src/gen)"

# Offline reference from the exact API/MCP projection; E2E detects drift.
api-reference:
	@reference_tmp=$$(mktemp); trap 'rm -f "$$reference_tmp"' EXIT; go run ./cmd/blaxsmith api-contract > "$$reference_tmp" && mv "$$reference_tmp" docs/machine-api-contract.json
