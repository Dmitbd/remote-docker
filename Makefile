.PHONY: test fmt vet

test:
	go test ./...

fmt:
	test -z "$$(gofmt -l .)"

vet:
	go vet ./...
