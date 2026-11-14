IMAGE ?= lws:latest
VERSION ?= dev
LDFLAGS := -s -w -X app/pkg/integration.Version=$(VERSION)

.PHONY: build test bench lint run docker docker-run clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/lws .

test:
	go test -race -count=1 ./...

bench:
	go test -run='^$$' -bench=. -benchmem ./...

lint:
	go vet ./...
	test -z "$$(gofmt -l .)"

run:
	go run . -config configs/config.local.json

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

docker-run:
	docker run --rm -p 8080:8080 -p 9090:9090 $(IMAGE)

clean:
	rm -rf bin/
