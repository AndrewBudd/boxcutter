.PHONY: help provision-node provision-orchestrator launch-node launch-orchestrator \
       stop-node stop-orchestrator ssh-node ssh-orchestrator status clean \
       build-host install-host

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

build-host:               ## Build boxcutter-host binary
	@cd host && go build -o boxcutter-host ./cmd/host/

install-host: build-host  ## Install boxcutter-host binary + systemd service
	@sudo cp host/boxcutter-host /usr/local/bin/boxcutter-host
	@sudo cp host/boxcutter-host.service /etc/systemd/system/
	@sudo systemctl daemon-reload
	@echo "Installed. Run: sudo systemctl enable --now boxcutter-host"

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

help:                     ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-28s\033[0m %s\n", $$1, $$2}'
