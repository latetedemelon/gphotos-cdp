BINARY := gphotos-cdp

.PHONY: all build test vet fmt fmt-check clean

all: fmt-check vet test build

build:
	go build -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-ed:"; echo "$$unformatted"; exit 1; \
	fi

clean:
	rm -f $(BINARY)
