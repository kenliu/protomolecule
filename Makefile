.PHONY: build clean test coverage

build:
	@mkdir -p bin
	go build -o bin/protomolecule .

clean:
	rm -f bin/protomolecule

test:
	go test -race -count=1 ./...

coverage:
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report written to coverage.html"
