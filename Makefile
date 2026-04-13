.PHONY: help provision-node provision-orchestrator launch-node launch-orchestrator \
       stop-node stop-orchestrator ssh-node ssh-orchestrator status clean \
       build-host install-host deb-host \
       build-image pull upgrade version provision-from-image \
       rebuild-tools deploy-tools \
       test vet check

# --- Orchestrator ---

provision-orchestrator:   ## Build orchestrator binaries + cloud-init ISO + VM disk
	@bash host/provision.sh orchestrator --rebuild

provision-orchestrator-iso: ## Rebuild orchestrator cloud-init ISO only
	@bash host/provision.sh orchestrator

launch-orchestrator:      ## Start the Orchestrator VM (foreground)
	@bash host/launch.sh orchestrator

launch-orchestrator-daemon: ## Start the Orchestrator VM (background)
	@bash host/launch.sh orchestrator --daemon

stop-orchestrator:        ## Stop the Orchestrator VM
	@bash host/stop.sh orchestrator

ssh-orchestrator:         ## SSH into the Orchestrator VM
	@ssh -i ~/.ssh/id_rsa -o StrictHostKeyChecking=no ubuntu@192.168.50.2

# --- Node ---

provision-node:           ## Build node binaries + cloud-init ISO + VM disk
	@bash host/provision.sh node --rebuild

provision-node-iso:       ## Rebuild node cloud-init ISO only
	@bash host/provision.sh node

launch-node:              ## Start a Node VM (foreground)
	@bash host/launch.sh node

launch-node-daemon:       ## Start a Node VM (background)
	@bash host/launch.sh node --daemon

stop-node:                ## Stop a Node VM
	@bash host/stop.sh node

ssh-node:                 ## SSH into Node VM 1 (use host/ssh.sh for others)
	@bash host/ssh.sh node 1

# --- Host Control Plane ---

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build-host:               ## Build boxcutter-host binary
	@cd host && go build -ldflags "-X main.version=$(VERSION)" -o boxcutter-host ./cmd/host/

install-host: build-host  ## Install boxcutter-host binary + systemd service
	@sudo cp host/boxcutter-host /usr/local/bin/boxcutter-host
	@sudo cp host/boxcutter-host.service /etc/systemd/system/
	@sudo sed -i 's|^#Environment=BOXCUTTER_REPO=.*|Environment=BOXCUTTER_REPO=$(CURDIR)|' /etc/systemd/system/boxcutter-host.service
	@sudo systemctl daemon-reload
	@echo "Installed with BOXCUTTER_REPO=$(CURDIR). Run: sudo systemctl enable --now boxcutter-host"

release-host: build-host  ## Create release tarball for boxcutter-host
	@mkdir -p .release
	@tar czf .release/boxcutter-host-$(VERSION)-linux-amd64.tar.gz \
		-C host boxcutter-host \
		-C .. host/boxcutter-host.service
	@echo "Release tarball: .release/boxcutter-host-$(VERSION)-linux-amd64.tar.gz"

deb-host:                 ## Build .deb package for boxcutter-host
	@bash host/build-deb.sh $(VERSION)

# --- OCI Images ---

publish:                  ## Build + push image to ghcr.io (usage: make publish TYPE=node)
	@bash host/publish-image.sh $(TYPE)

publish-all:              ## Build + push both node and orchestrator images
	@bash host/publish-image.sh all

build-image:              ## Build image locally only (usage: make build-image TYPE=node)
	@bash host/publish-image.sh $(TYPE) --build-only

pull:                     ## Pull image from OCI registry (usage: make pull TYPE=node)
	@cd host && go build -o boxcutter-host ./cmd/host/ && ./boxcutter-host pull $(TYPE) $(TAG)

version:                  ## Show running vs latest image versions
	@cd host && go build -o boxcutter-host ./cmd/host/ && ./boxcutter-host version

# --- Cluster ---

status:                   ## Show VM status
	@echo "=== Orchestrator ===" ; \
	if [ -f .images/orchestrator.pid ] && kill -0 $$(cat .images/orchestrator.pid) 2>/dev/null; then \
		echo "  running (PID $$(cat .images/orchestrator.pid))"; \
	else \
		echo "  stopped"; \
	fi ; \
	echo "=== Nodes ===" ; \
	for f in .images/boxcutter-node-*.pid; do \
		[ -f "$$f" ] || continue; \
		name=$$(basename "$$f" .pid); \
		if kill -0 $$(cat "$$f") 2>/dev/null; then \
			echo "  $$name: running (PID $$(cat "$$f"))"; \
		else \
			echo "  $$name: stopped (stale)"; \
		fi; \
	done

clean:                    ## Remove generated images (keeps cloud image download)
	rm -f .images/*.qcow2 .images/*-cloud-init.iso .images/*.pid .images/*.log

check-tag:                ## Validate version tag (usage: make check-tag TAG=v0.38.0)
	@bash scripts/check-version-tag $(TAG)

next-tag:                 ## Show next version tag (usage: make next-tag BUMP=patch)
	@bash scripts/check-version-tag --next $(or $(BUMP),patch)

install-tag-hook:         ## Install git pre-push hook to prevent tag regression
	@bash scripts/check-version-tag --install-hook

# --- Tools Image ---

rebuild-tools:            ## Rebuild tools.img (tapegun + CLI) without full reprovision
	@bash host/rebuild-tools.sh

deploy-tools:             ## Rebuild tools.img and deploy to all running nodes
	@bash host/rebuild-tools.sh --deploy

# --- Testing and Validation ---

test:                     ## Run tests for all Go modules
	@echo "Running tests for orchestrator..."
	@cd orchestrator && go test ./...
	@echo "Running tests for node/agent..."
	@cd node/agent && go test ./...
	@echo "Running tests for node/vmid..."
	@cd node/vmid && go test ./...
	@echo "Running tests for node/proxy..."
	@cd node/proxy && go test ./...
	@echo "Running tests for host..."
	@cd host && go test ./...

vet:                      ## Run go vet for all Go modules
	@echo "Running go vet for orchestrator..."
	@cd orchestrator && go vet ./...
	@echo "Running go vet for node/agent..."
	@cd node/agent && go vet ./...
	@echo "Running go vet for node/vmid..."
	@cd node/vmid && go vet ./...
	@echo "Running go vet for node/proxy..."
	@cd node/proxy && go vet ./...
	@echo "Running go vet for host..."
	@cd host && go vet ./...

check: test vet          ## Run both tests and vetting for all Go modules

help:                     ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-28s\033[0m %s\n", $$1, $$2}'
