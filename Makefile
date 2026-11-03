.PHONY: build test race bench fuzz backtest vet fmt run sim sim-all sim-trading sim-gap sim-reconnect

build:
	go build -o bin/ ./cmd/...

test:
	go test ./...

race:
	go test -race ./...

bench:
	go test -run '^$$' -bench . -benchmem ./...

FUZZTIME ?= 30s

fuzz:
	go test ./internal/oms -run '^$$' -fuzz '^FuzzDecimalRoundTrip$$' -fuzztime $(FUZZTIME)
	go test ./internal/audit -run '^$$' -fuzz '^FuzzRedact$$' -fuzztime $(FUZZTIME)
	go test ./internal/chaos -run '^$$' -fuzz '^FuzzFramer$$' -fuzztime $(FUZZTIME)
	go test ./internal/engine -run '^$$' -fuzz '^FuzzBook$$' -fuzztime $(FUZZTIME)

backtest:
	go run ./cmd/fixbt -steps 5000

vet:
	go vet ./...

fmt:
	gofmt -l -w .

run:
	go run ./cmd/fixgw -config deploy/fixgw.cfg -creds deploy/credentials.example.json

sim:
	go run ./cmd/fixsim -scenario orders

sim-trading:
	go run ./cmd/fixsim -scenario trading

sim-all:
	go run ./cmd/fixsim -scenario all

sim-gap:
	go run ./cmd/fixsim -scenario gap

sim-reconnect:
	go run ./cmd/fixsim -scenario reconnect
