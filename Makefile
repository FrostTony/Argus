BIN     := argus
# Empty unless git knows a tag.
VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null)
LDFLAGS := -s -w $(if $(VERSION),-X github.com/tonyamdfrost-cmd/Argus/internal/app.Version=$(VERSION))

.PHONY: build test race lint validate run docker clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/argus

test:
	go test ./...

race:
	go test -race ./...

lint:
	gofmt -l .
	go vet ./...

validate: build
	./$(BIN) validate -config examples/argus.yaml -probes examples/probes.d

run: build
	./$(BIN) run -config examples/argus.yaml -probes examples/probes.d

docker:
	docker build --build-arg VERSION=$(VERSION) -t argus:$(if $(VERSION),$(VERSION),latest) .

clean:
	rm -f $(BIN)
