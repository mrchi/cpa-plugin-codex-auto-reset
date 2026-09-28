# The host discovers plugins by file extension: .dylib on darwin, .so elsewhere (D5).
EXT := $(if $(filter Darwin,$(shell uname -s)),dylib,so)

.PHONY: build vet test clean

build:
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o dist/cpa-auto-reset.$(EXT) .

vet:
	go vet ./...

test:
	CGO_ENABLED=1 go test ./...

clean:
	rm -rf dist
