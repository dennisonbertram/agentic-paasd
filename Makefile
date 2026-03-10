.PHONY: build run clean

build:
	CGO_ENABLED=1 go build -o bin/paasd ./cmd/paasd

run: build
	./bin/paasd --port 8080 --db-path /var/lib/paasd/paasd.db --master-key-path /var/lib/paasd/master.key

clean:
	rm -rf bin/
