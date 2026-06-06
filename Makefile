.PHONY: build test vet clean docker-build docker-up docker-down docker-logs

build:
	go build -o bin/redmemo ./cmd/redmemo

test:
	go test ./... -count=1

vet:
	go vet ./...

clean:
	rm -rf bin/

docker-build:
	docker compose build

docker-up:
	docker compose up -d

docker-down:
	docker compose down

docker-logs:
	docker compose logs -f redmemo
