.PHONY: all build test run probe probe-transcript docker-build release docker-up deploy clean

VERSION ?= $(shell git rev-parse --short HEAD)

all: test build

build:
	go build -o server ./cmd/server

test:
	go test -v ./...

run: build
	./server

probe:
	go run ./cmd/scraper_probe/main.go

probe-transcript:
	go run ./cmd/transcript_probe/main.go

docker-build:
	docker build -t metacrawler:latest .

release:
	docker build --build-arg VERSION=$(VERSION) -t metacrawler:$(VERSION) .
	@echo "Built metacrawler:$(VERSION)"

docker-up:
	docker compose up -d

deploy:
	@if [ -f ./deploy.sh ]; then \
		./deploy.sh; \
	else \
		echo "Error: deploy.sh not found. Create it from deploy.example.sh"; \
		exit 1; \
	fi

clean:
	rm -f server
