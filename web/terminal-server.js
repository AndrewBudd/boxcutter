#!/usr/bin/env node
// WebSocket terminal server for Boxcutter VMs.
// Spawns SSH sessions to VMs and bridges them to browser terminals via WebSocket.
// Runs alongside the Next.js app on port 3001.

const { WebSocketServer } = require('ws')
const { spawn } = require('child_process')
const url = require('url')

const PORT = process.env.TERMINAL_PORT || 3001
const SSH_KEY = '/etc/boxcutter/secrets/cluster-ssh.key'
const ORCH_API = process.env.ORCHESTRATOR_API || 'http://localhost:8801'

const wss = new WebSocketServer({ port: PORT })
console.log(`Terminal server listening on port ${PORT}`)

// Look up the VM's node bridge IP and TAP name from the orchestrator/node APIs
async function getVMRoute(vmName) {
  try {
    const vmResp = await fetch(`${ORCH_API}/api/vms/${vmName}`)
    const vmData = await vmResp.json()
    const nodeId = vmData.node_id
    if (!nodeId) return null

    const nodesResp = await fetch(`${ORCH_API}/api/nodes`)
    const nodes = await nodesResp.json()
    const node = nodes.find(n => n.id === nodeId)
    if (!node || !node.bridge_ip) return null

    // Get TAP name from node agent
    const detailResp = await fetch(`http://${node.bridge_ip}:8800/api/vms/${vmName}`)
    const detail = await detailResp.json()
    const tap = detail.vm?.tap || detail.tap
    if (!tap) return null

    return { bridgeIP: node.bridge_ip, tap }
  } catch (e) {
    console.error(`Terminal: route lookup failed for ${vmName}:`, e.message)
    return null
  }
}

wss.on('connection', async (ws, req) => {
  const params = new URLSearchParams(url.parse(req.url).query)
  const vmName = params.get('vm')
  const ip = params.get('ip')

  if (!vmName || !ip) {
    ws.send('Error: vm and ip parameters required\r\n')
    ws.close()
    return
  }

  console.log(`Terminal: connecting to ${vmName} (${ip})`)

  // Try direct Tailscale SSH first, fall back to bridge+socat
  let sshCmd = `ssh -tt -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o SendEnv=LANG -i ${SSH_KEY} dev@${ip}`

  // Check if we can reach the VM directly via Tailscale
  const { execSync } = require('child_process')
  let useBridge = false
  try {
    execSync(`ping -c 1 -W 2 ${ip} 2>/dev/null`, { stdio: 'ignore' })
  } catch {
    // Can't reach via Tailscale, try bridge route
    const route = await getVMRoute(vmName)
    if (route) {
      console.log(`Terminal: using bridge route via ${route.bridgeIP} (${route.tap})`)
      sshCmd = `ssh -tt -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o SendEnv=LANG -i ${SSH_KEY} -o "ProxyCommand=ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i ${SSH_KEY} ubuntu@${route.bridgeIP} socat - TCP:10.0.0.2:22,so-bindtodevice=${route.tap}" dev@10.0.0.2`
      useBridge = true
    } else {
      console.log(`Terminal: can't reach ${vmName} via Tailscale or bridge`)
    }
  }

  // Spawn SSH with a PTY via script -qfc (gives us a real PTY on Linux)
  const ssh = spawn('script', [
    '-qfc',
    sshCmd,
    '/dev/null'
  ], {
    env: {
      ...process.env,
      TERM: 'xterm-256color',
      LANG: 'en_US.UTF-8',
      LC_ALL: 'en_US.UTF-8',
    },
  })

  ssh.stdout.on('data', (data) => {
    if (ws.readyState === 1) {
      ws.send(data.toString('utf8'))
    }
  })

  ssh.stderr.on('data', (data) => {
    if (ws.readyState === 1) {
      ws.send(data.toString('utf8'))
    }
  })

  ssh.on('close', (code) => {
    console.log(`Terminal: ${vmName} SSH exited (code ${code})`)
    if (ws.readyState === 1) {
      ws.send(`\r\nSSH session ended (exit code ${code})\r\n`)
      ws.close()
    }
  })

  ws.on('message', (data) => {
    const msg = data.toString()

    // Check for resize command
    try {
      const parsed = JSON.parse(msg)
      if (parsed.type === 'resize' && parsed.cols && parsed.rows) {
        // Send stty resize to the SSH session
        ssh.stdin.write(`stty cols ${parsed.cols} rows ${parsed.rows}\n`)
        return
      }
    } catch {
      // Not JSON, treat as terminal input
    }

    ssh.stdin.write(msg)
  })

  ws.on('close', () => {
    console.log(`Terminal: ${vmName} WebSocket closed`)
    ssh.kill()
  })

  ws.on('error', (err) => {
    console.error(`Terminal: ${vmName} WebSocket error:`, err.message)
    ssh.kill()
  })
})
