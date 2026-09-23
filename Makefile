.PHONY: test check build example web-check

test:
	go test ./...

check: test
	go vet ./...

build:
	go build -o bin/blaxsmith ./cmd/blaxsmith

example:
	go run ./cmd/blaxsmith check --recipe examples/guild/recipe.json --spec examples/guild/spec.md --transcript examples/guild/transcript.md --scope examples/guild

web-check:
	cd frontend && npm ci && npm run build
