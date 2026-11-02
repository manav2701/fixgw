.PHONY: build test race bench fuzz vet fmt run sim sim-gap sim-reconnect

build:
	go build -o bin/ ./cmd/...

test:
	go test ./...

race:
	go test -race ./...

bench:
	go test -run '^$$' -bench . -benchmem ./...

fuzz:
	go test -fuzz FuzzDecode -fuzztime 60s ./internal/codec

vet:
	go vet ./...

fmt:
	gofmt -l -w .

run:
	go run ./cmd/fixgw -config deploy/fixgw.cfg -creds deploy/credentials.example.json

sim:
	go run ./cmd/fixsim -scenario orders

sim-gap:
	go run ./cmd/fixsim -scenario gap

sim-reconnect:
	go run ./cmd/fixsim -scenario reconnect
