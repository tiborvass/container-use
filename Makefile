all: build

TARGETOS ?= linux
TARGETARCH ?= amd64

.PHONY: build
build:
	@which docker >/dev/null || ( echo "Please follow instructions to install Docker at https://docs.docker.com/get-started/get-docker/"; exit 1 )
	@docker build --build-arg TARGETOS=$(TARGETOS) --build-arg TARGETARCH=$(TARGETARCH) --platform local -o . .
	@ls cu

.PHONY: clean
clean:
	rm -f cu

.PHONY: find-path
find-path:
	@PREFERRED_DIR="$$HOME/.local/bin"; \
	if echo "$$PATH" | grep -q "$$PREFERRED_DIR"; then \
		echo "$$PREFERRED_DIR"; \
	else \
		for dir in $$(echo "$$PATH" | tr ':' ' '); do \
			if [ -w "$$dir" ]; then \
				echo "$$dir"; \
				break; \
			fi; \
		done; \
	fi

.PHONY: install
install: build
	@DEST=$$(make -s find-path | tail -n 1); \
	if [ -z "$$DEST" ]; then \
		echo "No writable directory found in \$PATH"; exit 1; \
	fi; \
	echo "Installing cu to $$DEST..."; \
	mv cu "$$DEST/"
