VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin/reproxy

# Packages that don't need Qt; testable anywhere, no display required.
CORE_PKGS := ./internal/browser/... ./internal/bodyview/... ./internal/capture/... ./internal/config/... ./internal/intercept/... ./internal/texttools/... ./internal/filter/... ./internal/syntaxcheck/... ./internal/editassist/... ./internal/elide/... ./internal/atomicfile/... ./internal/randid/... ./internal/replay/... ./internal/proxy/... ./internal/dns/... ./internal/tlsca/... ./internal/app/... ./internal/secrets/... ./internal/scope/... ./internal/focus/... ./internal/cli/... ./internal/instance/... ./internal/hub/... ./internal/agent/... ./internal/approval/... ./internal/mcp/... ./internal/engagement/... ./internal/archive/... ./internal/target/... ./internal/upstream/... ./internal/sitemap/... ./internal/files/... ./internal/findings/... ./internal/entities/... ./internal/websocket/... ./internal/crawl/... ./internal/terminal/... ./internal/plugin/... ./tools/...

# Package type for `make package`: deb, rpm, mac or win. All four are built
# out; mac has to run on macOS, and win needs makensis plus a mingw Qt.
TYPE ?= deb

.PHONY: build build-headless run test test-core test-ui print-core-pkgs vet fmt clean package icons deps-check vulncheck hooks

# -trimpath keeps the build machine's paths (home directory, module cache)
# out of the binary, and makes a release reproducible from its source.
# packaging/common.sh builds with the same flags, so both share one cache of
# the Qt bindings: change them together.
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/reproxy

# A Qt-free command-line build: the author tools (reproxy plugins …), the
# command line and the -headless server, with internal/ui left out by the
# `headless` tag. It needs no Qt toolchain, so CI that only runs the plugin
# tests — the reproxy-plugins repository's — builds this instead of the app.
build-headless:
	go build -trimpath -tags headless -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/reproxy

run: build
	./$(BIN) $(ARGS)

# -race is limited to the Qt-free packages: race-instrumenting the Qt
# bindings means a separate, very slow rebuild of them.
test: test-core test-ui

# The packages that need Qt: the first run compiles the bindings.
test-ui:
	go test ./internal/ui/... ./cmd/...

# Fast loop for proxy/capture work: skips compiling the Qt bindings.
test-core:
	go test -race $(CORE_PKGS)

# For CI, which vets the same packages it tests.
print-core-pkgs:
	@echo $(CORE_PKGS)

vet:
	go vet ./...

# Supply-chain checks (needs the network). deps-check refuses any module
# version, or Go toolchain, first published less than 14 days ago unless
# deps-soak.allow lists it with a reason; see tools/depsoak. It runs from
# the pre-commit hook whenever go.mod or go.sum change (`make hooks`).
deps-check:
	go run ./tools/depsoak

# govulncheck is pinned like everything else; its vulnerability data is
# fetched live, so the pin doesn't make the results stale.
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.8.0

vulncheck:
	go run $(GOVULNCHECK) ./...

# Points git at the repo's hooks (.githooks/).
hooks:
	git config core.hooksPath .githooks

fmt:
	gofmt -w .

# Builds an installable package into dist/. Pass the type and, optionally,
# the target architecture:
#
#	make package                  # a .deb for this machine
#	make package TYPE=deb ARCH=arm64
#	make package TYPE=mac         # a .dmg (on macOS)
#	make package TYPE=win         # an installer .exe
#
package:
	./packaging/package.sh $(TYPE) $(if $(ARCH),--arch $(ARCH)) $(if $(VERSION),--version $(VERSION))

# Re-renders assets/icons/*.png from assets/reproxy.svg. The PNGs are
# committed, so this is only needed after changing the SVG.
icons:
	./packaging/mkicons.sh

clean:
	rm -rf bin build dist
