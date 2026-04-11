# OpenCode Server Mode Integration Design

## Overview

Replace tmux send-keys automation with OpenCode server mode API calls to enable programmatic control of terminal sessions and agent tools.

## Current Usage of tmux send-keys

From the `boxcutter-tapegun` script, tmux send-keys is used in several contexts:
1. Setting up environment variables (`PATH`, `OPENAI_BASE_URL`, `OPENAI_API_KEY`)
2. Changing directories (`cd $CLAUDE_WORKING_DIR`)
3. Launching agent tools (`claude`, `opencode`, `aider`)
4. Executing tapegun sequences after agent startup
5. Injecting messages from the inbox into active terminals via `send_keys=true`

## OpenCode Server Mode Capabilities

The OpenCode server mode exposes an HTTP API that can be used for:
- Session management
- Message sending (equivalent to terminal command execution)
- Command execution
- File operations
邮寄

## Design Plan

### 1. Session Management

Instead of managing tmux sessions directly:
```bash
# Current approach
tmux send-keys -t main 'export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"' Enter

# Proposed approach
# Use OpenCode's session management via HTTP API
```

### 2. Command Execution

Replace terminal command automation with:
- `/sessions/{session_id}/messages` to send commands to Claude Code
- `/commands` endpoint to execute shell commands
- Direct API calls to OpenCode's agent interface

### 3. Environment Setup

Environment variables can be set using:
- OpenCode's configuration system through JSON files
- Environment variable passing in API calls
- Direct file writes to configuration files instead of tmux injection

### 4. Agent Tool Launch

Instead of tmux-based launches:
- Use OpenCode's HTTP API to start and configure agents
- Store configurations in `opencode.json` with API endpoints
- Use existing metadata service to manage state

### 5. Tapegun Sequences

Replace:
```bash
while IFS= read -r seq_line; do
    $TMUX_CMD send-keys -t 0 "$seq_line"
    sleep 0.5
    $TMUX_CMD send-keys -t 0 Enter
done
```

With:
- API calls to send messages to the active session
- Use OpenCode's `/messages` endpoint
- Implement proper timing and state management for sequential execution

### 6. Message Injection

Replace `send_keys=true` injection with:
- HTTP POST to OpenCode's message endpoint
- Use session information to target specific terminals  
- Leverage OpenCode's built-in inbox functionality

## Implementation Approach

1. **Configuration Migration**: Convert existing tmux-based environment setups to OpenCode configuration files or API calls

2. **Agent Launch Integration**: Replace direct tmux command launching with HTTP API calls to OpenCode agent management endpoints

3. **Sequence Execution**: Implement tapegun sequence execution through OpenCode's built-in messaging system

4. **Inbox Handling**: Replace tmux injection with direct OpenCode message sending through API

5. **Error Handling**: Implement robust error handling around API calls, with fallback to tmux-based methods if needed

## Benefits of Migration

- Eliminates dependency on tmux for automation
- Provides more reliable and stable command execution
- Enables better monitoring and logging of automated actions
- Leverages OpenCode's built-in agent communication features
- More secure (no direct terminal manipulation)
- Better integration with OpenCode's session management

## Technical Considerations

- Need to ensure API endpoints are available at runtime
- Maintain backward compatibility where possible
- Implement proper authentication for server API calls  
- Handle session state changes via API instead of tmux session tracking
- Ensure the server mode is properly configured and accessible