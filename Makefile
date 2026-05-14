BINARY    := mt
PKG       := .
PREFIX    ?= $(HOME)/.local
BIN_DIR   := $(PREFIX)/bin
APPS_DIR  := $(PREFIX)/share/applications

.PHONY: run build tidy clean install uninstall

run: tidy
	GDK_BACKEND=wayland go run $(PKG)

build: tidy
	go build -trimpath -ldflags="-s -w" -o $(BINARY) $(PKG)

tidy:
	@test -f go.sum || go mod tidy

clean:
	rm -f $(BINARY)

install: build
	install -Dm755 $(BINARY) $(BIN_DIR)/$(BINARY)
	install -Dm644 mt.desktop $(APPS_DIR)/mt.desktop
	@command -v update-desktop-database >/dev/null && update-desktop-database $(APPS_DIR) || true

uninstall:
	rm -f $(BIN_DIR)/$(BINARY) $(APPS_DIR)/mt.desktop
	@command -v update-desktop-database >/dev/null && update-desktop-database $(APPS_DIR) || true
