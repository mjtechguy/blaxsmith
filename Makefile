.PHONY: test check build example web-check proto-check

test:
	go test ./...

check: test
	go vet ./...

build:
	go build -o bin/blaxsmith ./cmd/blaxsmith

example:
	go run ./cmd/blaxsmith check --recipe examples/guild/recipe.json --spec examples/guild/spec.md --transcript examples/guild/transcript.md --scope examples/guild

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
