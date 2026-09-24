VERSION := 1.0

test:
	go test -race ./...

lint: fmt
	golangci-lint run

fmt:
	gofmt -w .
