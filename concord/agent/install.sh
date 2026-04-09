#!/bin/bash
# Installs the Concord task agent into a boxcutter VM.
# Deployed via: boxcutter cp-to <vm> install.sh /tmp/install.sh
# Run via:      boxcutter exec <vm> "bash /tmp/install.sh"
set -euo pipefail

AGENT_DIR="/opt/concord-agent"

sudo mkdir -p "${AGENT_DIR}"/{inbox,outbox,logs,workspace}
sudo chown -R dev:dev "${AGENT_DIR}"

cat > "${AGENT_DIR}/run-task.sh" << 'RUNNER'
#!/bin/bash
# Executes a task from the inbox directory.
# Task payload: /opt/concord-agent/inbox/task.json
# Status file:  /opt/concord-agent/status.json
# Output:       /opt/concord-agent/outbox/
# Log:          /opt/concord-agent/logs/task.log
set -uo pipefail

AGENT_DIR="/opt/concord-agent"
TASK_FILE="${AGENT_DIR}/inbox/task.json"
STATUS_FILE="${AGENT_DIR}/status.json"
LOG_FILE="${AGENT_DIR}/logs/task.log"
WORKSPACE="${AGENT_DIR}/workspace"

update_status() {
    local status="$1"
    local step="${2:-}"
    local exit_code="${3:-0}"
    local detail="${4:-}"
    cat > "${STATUS_FILE}" << EOF
{
  "status": "${status}",
  "step": "${step}",
  "exit_code": ${exit_code},
  "detail": "${detail}",
  "updated_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF
}

if [ ! -f "${TASK_FILE}" ]; then
    update_status "error" "" "1" "no task file found"
    echo "Error: ${TASK_FILE} not found" >&2
    exit 1
fi

# Parse task
TASK_ID=$(jq -r '.id // "unknown"' "${TASK_FILE}")
STEPS=$(jq -r '.steps | length' "${TASK_FILE}")

echo "=== Task ${TASK_ID}: ${STEPS} steps ===" > "${LOG_FILE}"
update_status "running" "" "0" "starting ${STEPS} steps"

# Execute each step sequentially
FAILED=false
for i in $(seq 0 $((STEPS - 1))); do
    STEP_NAME=$(jq -r ".steps[$i].name // \"step-$i\"" "${TASK_FILE}")
    STEP_CMD=$(jq -r ".steps[$i].command" "${TASK_FILE}")
    STEP_DIR=$(jq -r ".steps[$i].workdir // \"${WORKSPACE}\"" "${TASK_FILE}")

    echo "--- Step ${i}: ${STEP_NAME} ---" >> "${LOG_FILE}"
    update_status "running" "${STEP_NAME}" "0" "executing step $((i + 1))/${STEPS}"

    mkdir -p "${STEP_DIR}"
    STEP_EXIT=0
    bash -c "cd '${STEP_DIR}' && ${STEP_CMD}" >> "${LOG_FILE}" 2>&1 || STEP_EXIT=$?

    echo "--- Step ${i} exit code: ${STEP_EXIT} ---" >> "${LOG_FILE}"

    if [ "${STEP_EXIT}" -ne 0 ]; then
        update_status "failed" "${STEP_NAME}" "${STEP_EXIT}" "step $((i + 1))/${STEPS} failed"
        FAILED=true
        break
    fi
done

if [ "${FAILED}" = "false" ]; then
    update_status "completed" "" "0" "all ${STEPS} steps completed"
fi

# Collect output artifacts
if [ -d "${AGENT_DIR}/outbox" ] && [ "$(ls -A ${AGENT_DIR}/outbox 2>/dev/null)" ]; then
    echo "=== Output artifacts collected ===" >> "${LOG_FILE}"
fi

echo "=== Task finished at $(date -u +%Y-%m-%dT%H:%M:%SZ) ===" >> "${LOG_FILE}"
RUNNER

chmod +x "${AGENT_DIR}/run-task.sh"

# Write initial status
cat > "${AGENT_DIR}/status.json" << EOF
{
  "status": "ready",
  "step": "",
  "exit_code": 0,
  "detail": "agent installed",
  "updated_at": "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
}
EOF

echo "Concord agent installed at ${AGENT_DIR}"
