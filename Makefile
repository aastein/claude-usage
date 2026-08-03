BIN := claude-usage
PREFIX ?= $(HOME)/go/bin

APP := Claude Usage
APP_DIR := $(APP).app
IDENTIFIER := com.aastein.claude-usage
# Ad-hoc by default. For working notifications set IDENTITY to a
# "Developer ID Application: ..." certificate in your Keychain (macOS
# silently drops notifications from ad-hoc / unsigned bundles).
IDENTITY ?= -

.PHONY: build install clean bundle run-bundle install-bundle

build:
	go build -o $(BIN) .

install: build
	mkdir -p $(PREFIX)
	install -m 0755 $(BIN) $(PREFIX)/$(BIN)
	@echo "installed $(PREFIX)/$(BIN)"

# bundle assembles a menu bar .app: builds the binary into the bundle,
# writes an Info.plist (LSUIElement = no dock icon), and code-signs it.
bundle: build
	rm -rf "$(APP_DIR)"
	mkdir -p "$(APP_DIR)/Contents/MacOS"
	cp "$(BIN)" "$(APP_DIR)/Contents/MacOS/$(BIN)"
	@printf '%s\n' \
		'<?xml version="1.0" encoding="UTF-8"?>' \
		'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
		'<plist version="1.0">' \
		'<dict>' \
		'  <key>CFBundleExecutable</key><string>$(BIN)</string>' \
		'  <key>CFBundleIdentifier</key><string>$(IDENTIFIER)</string>' \
		'  <key>CFBundleName</key><string>$(APP)</string>' \
		'  <key>CFBundlePackageType</key><string>APPL</string>' \
		'  <key>CFBundleShortVersionString</key><string>0.1</string>' \
		'  <key>LSUIElement</key><true/>' \
		'  <key>NSHighResolutionCapable</key><true/>' \
		'</dict>' \
		'</plist>' > "$(APP_DIR)/Contents/Info.plist"
	codesign -f -s "$(IDENTITY)" "$(APP_DIR)"
	@echo "built $(APP_DIR)"

run-bundle: bundle
	open "$(APP_DIR)"

# install-bundle copies the app into /Applications.
install-bundle: bundle
	rm -rf "/Applications/$(APP_DIR)"
	cp -R "$(APP_DIR)" "/Applications/$(APP_DIR)"
	@echo "installed /Applications/$(APP_DIR)"

clean:
	rm -f $(BIN)
	rm -rf "$(APP_DIR)"
