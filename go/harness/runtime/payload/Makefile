PROVIDER ?= claude
PLATFORM ?= linux/amd64
IMAGE ?= kagent-runtime-$(PROVIDER):local
.PHONY: build
build:
	docker build --platform $(PLATFORM) --build-arg PROVIDER=$(PROVIDER) -f Dockerfile -t $(IMAGE) ../../..
