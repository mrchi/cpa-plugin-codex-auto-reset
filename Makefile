ID      := cpa-plugin-codex-auto-reset
VERSION ?= $(shell sed -n 's/^[[:space:]]*pluginVersion = "\(.*\)"/\1/p' main.go)
GOOS    ?= $(shell go env GOOS)
GOARCH  ?= $(shell go env GOARCH)
# The host discovers plugins by file extension: .dylib on darwin, .so elsewhere (D5).
EXT     := $(if $(filter darwin,$(GOOS)),dylib,so)

LIB     := $(ID).$(EXT)
ARCHIVE := $(ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip

.PHONY: build vet test release verify-release print-version clean

build:
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -buildmode=c-shared -o dist/$(LIB) .

vet:
	go vet ./...

test:
	CGO_ENABLED=1 go test ./...

# print-version exposes the derivation to CI, so the tag check reads it from the same
# place the archive name does instead of parsing main.go a second time.
print-version:
	@echo $(VERSION)

# release builds one platform's archive in the layout CPA's plugin store expects: the
# asset is <id>_<version>_<goos>_<goarch>.zip and carries the library at its root, named
# <id>.<ext>, with a matching checksums.txt line. Call it once per platform into the same
# dist/ to assemble a multi-platform release: dist/*.zip plus one checksums.txt.
release: build
	rm -f dist/$(ARCHIVE)
	cd dist && zip -jq $(ARCHIVE) $(LIB)
	cd dist && shasum -a 256 $(ARCHIVE) >> checksums.txt
	$(MAKE) --no-print-directory verify-release

# verify-release checks an already-built archive against the store's contract
# (SelectReleaseAssets + readTargetLibrary), so a bad zip fails the build here rather
# than silently in a panel install.
verify-release:
	@test -n '$(VERSION)' || { echo "cannot derive VERSION from main.go pluginVersion" >&2; exit 1; }
	@unzip -l dist/$(ARCHIVE) | awk '{print $$4}' | grep -qxF '$(LIB)' \
		|| { echo "release check failed: $(ARCHIVE) must carry $(LIB) at its root" >&2; exit 1; }
	@awk '{print $$2}' dist/checksums.txt | grep -qxF '$(ARCHIVE)' \
		|| { echo "release check failed: checksums.txt has no entry for $(ARCHIVE)" >&2; exit 1; }

clean:
	rm -rf dist
