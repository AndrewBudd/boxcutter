#!/usr/bin/env node
// WebSocket terminal server for Boxcutter VMs.
// Spawns SSH sessions to VMs and bridges them to browser terminals via WebSocket.
// Runs alongside the Next.js app on port 3001.

const { WebSocketServer } = require('ws')
const { spawn } = require('child_process')
const url = require('url')

const PORT = process.env.TERMINAL_PORT || 3001
const SSH_KEY = '/etc/boxcutter/secrets/cluster-ssh.key'

const wss = new WebSocketServer({ port: PORT })
console.log(`Terminal server listening on port ${PORT}`)

wss.on('connection', (ws, req) => {
  const params = new URLSearchParams(url.parse(req.url).query)
  const vmName = params.get('vm')
  const ip = params.get('ip')

  if (!vmName || !ip) {
    ws.send('Error: vm and ip parameters required\r\n')
    ws.close()
    return
  }

  console.log(`Terminal: connecting to ${vmName} (${ip})`)

  // Spawn SSH with a PTY via script -qfc (gives us a real PTY on Linux)
  const ssh = spawn('script', [
    '-qfc',
    `ssh -tt -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i ${SSH_KEY} dev@${ip}`,
    '/dev/null'
  ], {
    env: { ...process.env, TERM: 'xterm-256color' },
  })

  ssh.stdout.on('data', (data) => {
    if (ws.readyState === 1) {
      ws.send(data.toString('binary'))
    }
  })

  ssh.stderr.on('data', (data) => {
    if (ws.readyState === 1) {
      ws.send(data.toString('binary'))
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
