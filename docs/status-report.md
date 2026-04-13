# v0.49.0 Status Report

## What Shipped

This release focused on stability improvements and bug fixes:

1. **Bug Fixes**:
   - Fixed CI: clean root-owned artifacts before checkout
   - Fixed double-prefixed queue names on team apply
   - Fixed vmid restart issue: re-register VMs when vmid restarts

2. **Documentation Improvements**:
   - Added v0.48.0 status report summarizing messaging implementation and roadmap
   - Added messaging system documentation for SSH CLI commands

## What's Open

Based on the prioritized backlog, these items remain in progress:

1. **#176 - Local GPU Inference** - LiteLLM and Ollama config are done, only GPU hardware recovery needed

2. **Consolidated Credentials & Login** - Combining credential delivery via cloud-init with login streamlining

3. **#80 - E2E Validation** - End-to-end validation testing

4. **Messaging System Enhancements** - Building on the core messaging implementation from v0.48.0:
   - Further enhancements to message lifecycle management
   - Additional API endpoints and features based on user feedback

## What's Next

The upcoming priorities for v0.50.0 include:

1. Complete the local GPU inference implementation by recovering GPU hardware support
2. Finalize the consolidated credentials and login system
3. Conduct comprehensive end-to-end validation testing
4. Enhance messaging system with additional features based on user feedback
5. Improve documentation and examples for the new messaging capabilities

This release focused on stability and bug fixes, setting the stage for the next set of feature enhancements in v0.50.0.