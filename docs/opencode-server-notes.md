# OpenCode Server Mode Summary

## Overview
OpenCode server mode (`opencode serve`) runs a headless HTTP server that exposes an OpenAPI endpoint radiant for programmatic interaction with OpenCode. It enables multiple clients and allows programmatic control through HTTP APIs.

## Key Features

### Server Launch
- Command: `opencode serve [--port <number>] [--hostname <string>] [--cors <origin>]`
- Default port: 4096
- Default hostname: 127.0.0.1
- Authentication: HTTP basic auth with `OPENCODE_SERVER_PASSWORD` (username defaults to `opencode`)

### APIs Available

#### Sessions Management
- Create, list, get, update, delete sessions
- Session status tracking
- Fork sessions from messages
- Session analysis and summarization
- Permission request handling

#### Messages & Communication
- Send messages and wait for responses
- Send messages asynchronously
- Execute slash commands
- Run shell commands through the server
- Access session message history

#### Project & File Operations
- List projects and get current project info
- File searching and content retrieval
- VCS (Version Control System) information
- File status tracking

#### System & Configuration
- Get server health and version
- Get server configuration
- Manage providers and authentication
- Get available agents and commands
- Access LSP, formatters, and MCP servers

#### TUI Control
- Control terminal user interface programmatically
- Append text to prompt, submit prompts
- Open various selectors (sessions, themes, models)
- Show toasts and handle control requests

## Architecture Benefits
- Separation of UI (TUI) and server components
- Multiple clients can connect to same server
- Enables automation and integration with external tools
- Provides OpenAPI specification for SDK generation
- Supports mDNS discovery for easier client discovery

## Use Cases for Integration
- Automating terminal interactions in VM environments
- Programmatic control of OpenCode sessions
- External tool integration with OpenCode capabilities
- CI/CD pipeline automation using OpenCode's agent system
- Remote session management and monitoring