# `set -a` exports what .env defines; plain `.` will not, and os.Getenv sees nothing.
ENV = set -a && . ./.env && set +a

.PHONY: help test test-db lint fmt vet cover audit example example-generate check

help:
	@echo "test            go test ./...            (TestCRUD SKIPS — see test-db)"
	@echo "test-db         same, with .env exported (TestCRUD runs)"
	@echo "lint            golangci-lint run"
	@echo "fmt             gofmt -w ."
	@echo "vet             go vet ./..."
	@echo "cover           coverage profile + summary"
	@echo "audit           govulncheck both modules"
	@echo "example         build examples/quickstart, check for unfilled stubs"
	@echo "example-generate  re-run gqlgen in the example"
	@echo "check           fmt check + vet + lint + audit + test-db + example"

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

# Both modules, because they have different dependency graphs — the example pulls in gqlgen's CLI
# through its tool directive, and the library does not. luima's whole job is wiring four other
# libraries together, so a CVE in any of them is luima's problem whether or not luima's own code
# changed.
audit:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...
	cd examples/quickstart && go run golang.org/x/vuln/cmd/govulncheck@latest ./...

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
	@$(MAKE) audit
	@$(MAKE) test-db
	@$(MAKE) example
