# `set -a` exports what .env defines; plain `.` will not, and os.Getenv sees nothing.
ENV = set -a && . ./.env && set +a

.PHONY: help test test-db lint fmt vet cover audit example example-generate luimagen-roundtrip check

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
	@echo "luimagen-roundtrip  run luimagen against a scratch copy of the example (needs gqlgen)"
	@echo "check           fmt check + vet + lint + audit + test-db + example + luimagen-roundtrip"

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

# The only check that runs the gqlgen half of luimagen. internal_test.go is deliberately no-exec,
# so runGqlgenGenerate, inputFieldNames against a real models_gen.go and patchStubs against real
# gqlgen output are covered by nothing else — which is how two naming bugs stayed green under a
# passing suite. A scratch copy, never the committed example: Generate rewrites the schema, the
# model directory and the resolver file in place. ApiKey rather than User on purpose — gqlgen
# renames ApiKeyInput to APIKeyInput and OwnerId to OwnerID through templates.ToGo, and that drift
# is what this catches. Two tars through a file rather than a pipe: `a | b || exit 1` only ever
# checks b, and /bin/sh has no pipefail to fix that portably — a producer that died partway would
# silently leave the round trip running against a truncated copy of the repo.
luimagen-roundtrip:
	@scratch=$$(mktemp -d) || exit 1; trap 'rm -rf "$$scratch"' EXIT; \
	tar -cf "$$scratch/repo.tar" --exclude=.git . || exit 1; \
	tar -xf "$$scratch/repo.tar" -C "$$scratch" || exit 1; \
	rm -f "$$scratch/repo.tar"; \
	cd "$$scratch/examples/quickstart" || exit 1; \
	go run github.com/ulas96/luima/cmd/luimagen -type ApiKey -table api_keys \
	  -field ApiKeyID:string:pk -field OwnerId:string -field 'Scopes:[]string' || exit 1; \
	go build ./... || exit 1; \
	go vet ./... || exit 1; \
	if grep -rn 'not implemented' graph/*.resolvers.go; then \
		echo '^^ luimagen left an unfilled stub — these COMPILE and panic at runtime'; exit 1; \
	fi; \
	echo 'luimagen round trip ok'

check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }
	@$(MAKE) vet
	@$(MAKE) lint
	@$(MAKE) audit
	@$(MAKE) test-db
	@$(MAKE) example
	@$(MAKE) luimagen-roundtrip
