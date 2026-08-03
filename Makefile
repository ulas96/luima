# `set -a` exports what .env defines; plain `.` will not, and os.Getenv sees nothing.
ENV = set -a && . ./.env && set +a

.PHONY: help test test-db lint fmt vet cover example example-generate check

help:
	@echo "test            go test ./...            (TestCRUD SKIPS — see test-db)"
	@echo "test-db         same, with .env exported (TestCRUD runs)"
	@echo "lint            golangci-lint run"
	@echo "fmt             gofmt -w ."
	@echo "vet             go vet ./..."
	@echo "cover           coverage profile + summary"
	@echo "example         build examples/quickstart, check for unfilled stubs"
	@echo "example-generate  re-run gqlgen in the example"
	@echo "check           fmt check + vet + lint + test-db + example"

test:
	go test ./...

# The only run that proves anything: TestCRUD skips silently without DATABASE_URL, and a skipped
# test still reports ok. -v so you can see that it ran.
test-db:
	$(ENV) && go test -v -count=1 ./...

lint:
	golangci-lint run

fmt:
	gofmt -w .

vet:
	go vet ./...

cover:
	$(ENV) && go test -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out | tail -1

example:
	cd examples/quickstart && go build ./...
	@grep -rn 'not implemented' examples/quickstart/graph/*.resolvers.go \
		&& echo '^^ unfilled resolver stubs — these COMPILE and panic at runtime; go build will not catch them' \
		&& exit 1 || true

example-generate:
	cd examples/quickstart && go tool gqlgen generate
	@$(MAKE) example

check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	@$(MAKE) vet
	@$(MAKE) lint
	@$(MAKE) test-db
	@$(MAKE) example
