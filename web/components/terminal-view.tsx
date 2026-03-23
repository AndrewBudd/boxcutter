'use client'

import { useEffect, useRef, useState } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'

export default function TerminalView({ vmName, tailscaleIP }: { vmName: string; tailscaleIP: string }) {
  const termRef = useRef<HTMLDivElement>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const [connected, setConnected] = useState(false)

  const sendText = (text: string) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(text)
    }
  }

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
      rows: 24,
      cols: 100,
    })
    const fitAddon = new FitAddon()
    terminal.loadAddon(fitAddon)
    terminal.open(termRef.current)
    fitAddon.fit()

    terminal.writeln(`Connecting to ${vmName} (${tailscaleIP})...`)

    const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
    const wsHost = window.location.host
    const ws = new WebSocket(`${proto}://${wsHost}/ws?vm=${vmName}&ip=${tailscaleIP}`)
    wsRef.current = ws

    ws.onopen = () => {
      terminal.writeln('Connected.\r\n')
      setConnected(true)
      ws.send(JSON.stringify({ type: 'resize', cols: terminal.cols, rows: terminal.rows }))
    }

    ws.onmessage = (event) => terminal.write(event.data)

    ws.onerror = () => {
      terminal.writeln('\r\n\x1b[31mWebSocket error\x1b[0m')
      setConnected(false)
    }

    ws.onclose = () => {
      terminal.writeln('\r\n\x1b[33mDisconnected.\x1b[0m')
      setConnected(false)
    }

    terminal.onData((data) => {
      if (ws.readyState === WebSocket.OPEN) ws.send(data)
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
    <div>
      {/* Control buttons */}
      <div className="flex flex-wrap gap-2 mb-2">
        <button onClick={() => sendText('tmux attach\n')}
          className="px-3 py-1.5 bg-green-800 hover:bg-green-700 rounded text-xs text-white font-medium">
          tmux attach
        </button>
        <button onClick={() => sendText('\x02d')}
          className="px-3 py-1.5 bg-yellow-800 hover:bg-yellow-700 rounded text-xs text-white font-medium">
          tmux detach
        </button>
        <div className="border-l border-gray-700 mx-1" />
        <button onClick={() => sendText('\x1b[A')} className="px-3 py-1.5 bg-gray-800 hover:bg-gray-700 rounded text-xs">
          ↑
        </button>
        <button onClick={() => sendText('\x1b[B')} className="px-3 py-1.5 bg-gray-800 hover:bg-gray-700 rounded text-xs">
          ↓
        </button>
        <button onClick={() => sendText('\x1b[D')} className="px-3 py-1.5 bg-gray-800 hover:bg-gray-700 rounded text-xs">
          ←
        </button>
        <button onClick={() => sendText('\x1b[C')} className="px-3 py-1.5 bg-gray-800 hover:bg-gray-700 rounded text-xs">
          →
        </button>
        <div className="border-l border-gray-700 mx-1" />
        <button onClick={() => sendText('\x03')} className="px-3 py-1.5 bg-red-900 hover:bg-red-800 rounded text-xs">
          Ctrl+C
        </button>
        <button onClick={() => sendText('\n')} className="px-3 py-1.5 bg-gray-800 hover:bg-gray-700 rounded text-xs">
          Enter
        </button>
        <div className="flex-1" />
        <span className={'text-xs px-2 py-1.5 rounded ' + (connected ? 'text-green-400' : 'text-red-400')}>
          {connected ? 'connected' : 'disconnected'}
        </span>
      </div>

      {/* Terminal */}
      <div className="bg-[#0d1117] rounded-lg border border-gray-800 p-1">
        <div ref={termRef} />
      </div>
    </div>
  )
}
