.PHONY: build run clean

build:
	CGO_ENABLED=1 go build -o bin/ah ./cmd/ah

run: build
	./bin/ah --port 8080 --db-path /var/lib/ah/ah.db --master-key-path /var/lib/ah/master.key

clean:
	rm -rf bin/
