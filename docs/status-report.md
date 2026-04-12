# v0.48.0 Status Report

## What Shipped

This release focused heavily on messaging infrastructure and credential delivery improvements:

1. **Unified Messaging System (#190)** - Implemented a complete VM-to-VM messaging system with:
   - Data model for unified topics/queues
   - HTTP API endpoints for message sending and consumption
   - SSH commands for messaging operations
   - Cross-node message routing through the control plane
   - Auto-creation of messaging queues and topics on team apply

2. **Claude Code Integration**:
   - Added Claude Code credential delivery via team YAML
   - Integrated queue-based messaging with status publishing for Claude Code plugin
   - Connected queue messages to OpenCode API instead of stdout
   - Added queue-to-OpenCode API bridge in tapegun main loop

3. **Boot Agent**:
   - Added BootAgent for VM self-configuration (#185)

4. **CI Improvements**:
   - Fixed CI golden builds without loop mount (#184)
   - Speed up CI by building node and orchestrator images in parallel

5. **Bug Fixes**:
   - Fixed vmid dropping tool and endpoint fields from agent config
   - Fixed agent_config forwarding in orchestrator
   - Fixed QCOW2 write persistence and tools.img mount reliability
   - Fixed local filesystem mounting issues

## What's Open

Based on the prioritized backlog, these items remain in progress:

1. **#190 - Unified Messaging** - While core messaging is implemented, ongoing work includes:
   - Further enhancements to message lifecycle management
   - Additional API endpoints and features

2. **#176 - Local GPU Inference** - LiteLLM and Ollama config are done, only GPU hardware recovery needed

3. **Consolidated Credentials & Login** - Combining credential delivery via cloud-init with login streamlining

4. **#80 - E2E Validation** - End-to-end validation testing

## What's Next

The upcoming priorities for v0.49.0 include:

1. Complete the local GPU inference implementation by recovering GPU hardware support
2. Finalize the consolidated credentials and login system
3. Conduct comprehensive end-to-end validation testing
4. Enhance messaging system with additional features based on early user feedback
5. Improve documentation and examples for the new messaging capabilities

This release represents a significant step forward in inter-VM communication capabilities and integration with Claude Code, setting the stage for more sophisticated distributed workflows.