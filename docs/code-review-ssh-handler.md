# Code Review: Orchestrator SSH Handler

## Overview
The SSH handler (`orchestrator/internal/ssh/handler.go`) is a large file with 2,970 lines that serves as the primary interface for user interactions with the boxcutter system via SSH commands.

## Size and Structure
- **Total lines**: 2,970
- **Function count**: 60+ functions
- **Largest functions**:
  - `teamApply`: 255 lines
  - `cmdMsg`: 198 lines
  - `printHelp`: 83 lines
  - `teamDiff`: ~160 lines (lines 1204-1343)

## Code Quality Issues

### 1. Function Size Violations
Several functions exceed 100 lines, violating the principle of single responsibility:
- `teamApply` (255 lines) - Handles parsing, validation, VM creation, updates, and destruction
- `cmdMsg` (198 lines) - Contains all messaging subcommands in one function
- `teamDiff` (~160 lines) - Complex diffing logic for team configurations

### 2. Duplication
- **Error handling patterns**: Repeated `fmt.Fprintf(os.Stderr, "Error: ...")` patterns (11 occurrences)
- **JSON unmarshaling**: 50+ instances of `json.Unmarshal` with similar error handling
- **Team command structure**: Multiple team-related functions with similar patterns for API calls and response handling

### 3. Maintainability Concerns
- The single file contains all SSH command handling, making it difficult to navigate
- Related functionality (team commands, messaging, queue management) is not modularized
- HTTP helper methods (`get`, `post`, `postStream`, etc.) are mixed with business logic

## Recommendations

### 1. Split Large Functions
- Break `teamApply` into smaller functions for parsing, VM creation, updates, and destruction
- Split `cmdMsg` into separate handler functions for each subcommand
- Extract the help text generation to a separate function or file

### 2. Modularize Related Commands
- Create separate packages for:
  - Team commands (`team/`)
  - Messaging commands (`messaging/`)
  - Queue commands (`queue/`)
  - Tapegun commands (`tapegun/`)

### 3. Standardize Error Handling
- Create helper functions for common error handling patterns
- Implement consistent JSON unmarshaling with error handling

### 4. Improve Testability
- Extract business logic from HTTP handler functions
- Create interfaces for external dependencies to enable mocking

## Conclusion
The SSH handler serves as a critical component but suffers from being a monolithic file. Breaking it into smaller, focused modules would significantly improve maintainability and testability while adhering to single responsibility principles.