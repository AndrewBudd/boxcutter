.PHONY: setup launch stop ssh status clean

# --- Physical host targets ---

setup:                ## Install QEMU, download cloud image, create TAP device + NAT
	@bash host/setup.sh

launch:               ## Start the Boxcutter Node VM (foreground)
	@bash host/launch.sh

launch-daemon:        ## Start the Boxcutter Node VM (background)
	@bash host/launch.sh --daemon

stop:                 ## Stop the Node VM
	@bash host/stop.sh

ssh:                  ## SSH into the Node VM
	@bash host/ssh.sh

status:               ## Show Node VM status
	@if [ -f .images/node.pid ] && kill -0 $$(cat .images/node.pid) 2>/dev/null; then \
		echo "Node VM: running (PID $$(cat .images/node.pid))"; \
	else \
		echo "Node VM: stopped"; \
	fi

clean:                ## Remove generated images (keeps cloud image download)
	rm -f .images/node.qcow2 .images/cloud-init.iso .images/node.pid

help:                 ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'
