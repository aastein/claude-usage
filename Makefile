BIN := claude-usage
PREFIX ?= $(HOME)/go/bin

.PHONY: build install clean

build:
	go build -o $(BIN) .

install: build
	mkdir -p $(PREFIX)
	install -m 0755 $(BIN) $(PREFIX)/$(BIN)
	@echo "installed $(PREFIX)/$(BIN)"

clean:
	rm -f $(BIN)
