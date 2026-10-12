.PHONY: build test vet dist clean
build:
	go build -o bin/loadmeter ./cmd/loadmeter
test:
	go test ./...
vet:
	go vet ./...
dist:
	./scripts/dist.sh
clean:
	rm -rf bin dist
