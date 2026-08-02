.PHONY: build test run-mock docker-build

build:
	go build ./cmd/server

test:
	go test ./...

run-mock:
	DEV_MOCK=true AUTH_TOKEN=dev-local-token COOKIE_SECURE=false go run ./cmd/server

docker-build:
	docker build -t vertex-chat:local .

