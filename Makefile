.PHONY: all build test run docker-build docker-up clean

all: test build

build:
	go build -o server ./cmd/server

test:
	go test -v ./...

run: build
	./server

probe:
	go run ./cmd/scraper_probe/main.go

docker-build:
	docker build -t metacrawler:latest .

docker-up:
	docker compose up -d

clean:
	rm -f server
