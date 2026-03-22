'use client'

import { useEffect, useRef } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'

export default function TerminalView({ vmName, tailscaleIP }: { vmName: string; tailscaleIP: string }) {
  const termRef = useRef<HTMLDivElement>(null)
  const terminalRef = useRef<Terminal | null>(null)

  useEffect(() => {
    if (!termRef.current) return

    const terminal = new Terminal({
      cursorBlink: true,
      fontSize: 13,
      fontFamily: "'JetBrains Mono', 'Fira Code', 'Cascadia Code', monospace",
      theme: {
        background: '#0d1117',
        foreground: '#c9d1d9',
        cursor: '#58a6ff',
        selectionBackground: '#264f78',
      },
      rows: 30,
      cols: 120,
    })
    const fitAddon = new FitAddon()
    terminal.loadAddon(fitAddon)
    terminal.open(termRef.current)
    fitAddon.fit()
    terminalRef.current = terminal

    terminal.writeln(`Connecting to ${vmName} (${tailscaleIP})...`)

    // Connect WebSocket through nginx (wss on 443, or ws on 3001 for dev)
    const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
    const wsHost = window.location.host // includes port if non-standard
    const ws = new WebSocket(`${proto}://${wsHost}/ws?vm=${vmName}&ip=${tailscaleIP}`)

    ws.onopen = () => {
      terminal.writeln('Connected.\r\n')
      // Send terminal size
      ws.send(JSON.stringify({ type: 'resize', cols: terminal.cols, rows: terminal.rows }))
    }

    ws.onmessage = (event) => {
      terminal.write(event.data)
    }

    ws.onerror = () => {
      terminal.writeln('\r\n\x1b[31mWebSocket error — is the terminal server running on port 3001?\x1b[0m')
    }

    ws.onclose = () => {
      terminal.writeln('\r\n\x1b[33mDisconnected.\x1b[0m')
    }

    terminal.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(data)
      }
    })

    terminal.onResize(({ cols, rows }) => {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'resize', cols, rows }))
      }
    })

    const resizeObserver = new ResizeObserver(() => fitAddon.fit())
    resizeObserver.observe(termRef.current)

    return () => {
      ws.close()
      terminal.dispose()
      resizeObserver.disconnect()
    }
  }, [vmName, tailscaleIP])

  return (
    <div className="bg-[#0d1117] rounded-lg border border-gray-800 p-1">
      <div ref={termRef} />
    </div>
  )
}
