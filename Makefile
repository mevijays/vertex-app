.PHONY: build test run-mock docker-build

build:
	go build ./cmd/server

test:
	go test ./...

run-mock:
	DEV_MOCK=true go run ./cmd/server

docker-build:
	docker build -t vertex-chat:local .

