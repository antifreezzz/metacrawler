.PHONY: all build test run docker-build docker-up deploy clean

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

deploy:
	@if [ -f ./deploy.sh ]; then \
		./deploy.sh; \
	else \
		echo "Error: deploy.sh not found. Create it from deploy.example.sh"; \
		exit 1; \
	fi

clean:
	rm -f server
