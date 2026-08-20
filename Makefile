BINARIES := hub-server cluster-agent

.PHONY: build build-hub-server build-cluster-agent run-hub-server run-cluster-agent test lint clean

build: $(addprefix build-,$(BINARIES))

build-hub-server:
	go build -o bin/hub-server ./cmd/hub-server

build-cluster-agent:
	go build -o bin/cluster-agent ./cmd/cluster-agent

run-hub-server: build-hub-server
	./bin/hub-server

run-cluster-agent: build-cluster-agent
	./bin/cluster-agent

test:
	go test ./...

lint:
	go vet ./...

clean:
	rm -rf bin
