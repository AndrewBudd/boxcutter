'use client'

import { useEffect, useRef, useState, useCallback } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'

export default function TerminalView({ vmName, tailscaleIP }: { vmName: string; tailscaleIP: string }) {
  const termRef = useRef<HTMLDivElement>(null)
  const terminalRef = useRef<Terminal | null>(null)
  const fitAddonRef = useRef<FitAddon | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const [connected, setConnected] = useState(false)

  const sendText = (text: string) => {
    if (wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(text)
    }
  }

  const connect = useCallback(() => {
    const terminal = terminalRef.current
    if (!terminal) return

    // Close existing connection
    if (wsRef.current) {
      wsRef.current.close()
      wsRef.current = null
    }

    terminal.writeln(`\x1b[2m Connecting to ${vmName}...\x1b[0m`)

    const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
    const wsHost = window.location.host
    const ws = new WebSocket(`${proto}://${wsHost}/ws?vm=${vmName}&ip=${tailscaleIP}`)
    wsRef.current = ws

    ws.onopen = () => {
      terminal.writeln(`\x1b[32m Connected.\x1b[0m\r\n`)
      setConnected(true)
      ws.send(JSON.stringify({ type: 'resize', cols: terminal.cols, rows: terminal.rows }))
    }

    ws.onmessage = (event) => terminal.write(event.data)

    ws.onerror = () => {
      terminal.writeln('\r\n\x1b[31m Connection error.\x1b[0m')
      setConnected(false)
    }

    ws.onclose = () => {
      terminal.writeln('\r\n\x1b[33m Session ended.\x1b[0m')
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
  }, [vmName, tailscaleIP])

  useEffect(() => {
    if (!termRef.current) return

    const terminal = new Terminal({
      cursorBlink: true,
      fontSize: 13,
      fontFamily: "'JetBrains Mono', 'Fira Code', monospace",
      theme: {
        background: '#0d1117',
        foreground: '#e6edf3',
        cursor: '#58a6ff',
        selectionBackground: '#264f78',
        black: '#0d1117',
        red: '#ff7b72',
        green: '#7ee787',
        yellow: '#d29922',
        blue: '#58a6ff',
        magenta: '#bc8cff',
        cyan: '#76e3ea',
        white: '#e6edf3',
        brightBlack: '#484f58',
        brightRed: '#ffa198',
        brightGreen: '#56d364',
        brightYellow: '#e3b341',
        brightBlue: '#79c0ff',
        brightMagenta: '#d2a8ff',
        brightCyan: '#b3f0ff',
        brightWhite: '#f0f6fc',
      },
      rows: 24,
      cols: 100,
    })
    const fitAddon = new FitAddon()
    terminal.loadAddon(fitAddon)
    terminal.open(termRef.current)
    fitAddon.fit()

    terminalRef.current = terminal
    fitAddonRef.current = fitAddon

    const resizeObserver = new ResizeObserver(() => fitAddon.fit())
    resizeObserver.observe(termRef.current)

    // Initial connection
    connect()

    return () => {
      wsRef.current?.close()
      terminal.dispose()
      resizeObserver.disconnect()
      terminalRef.current = null
    }
  }, [connect])

  const reconnect = () => {
    const terminal = terminalRef.current
    if (!terminal) return
    terminal.writeln('\r\n\x1b[2m Reconnecting...\x1b[0m')
    connect()
  }

  return (
    <div>
      {/* Toolbar */}
      <div className="flex flex-wrap items-center gap-1.5 mb-2">
        <div className="flex gap-1">
          <button onClick={() => sendText('tmux attach\n')} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-emerald-600 hover:bg-emerald-500 text-white transition-all active:scale-95 text-[11px]">tmux attach</button>
          <button onClick={() => sendText('\x02d')} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-amber-600 hover:bg-amber-500 text-white transition-all active:scale-95 text-[11px]">tmux detach</button>
        </div>
        <div className="w-px h-5 bg-white/10 mx-1" />
        <div className="flex gap-1">
          {[['↑', '\x1b[A'], ['↓', '\x1b[B'], ['←', '\x1b[D'], ['→', '\x1b[C']].map(([label, code]) => (
            <button key={label} onClick={() => sendText(code)} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-gray-800 hover:bg-gray-700 text-gray-300 transition-all active:scale-95 w-8 text-center text-[11px]">{label}</button>
          ))}
        </div>
        <div className="w-px h-5 bg-white/10 mx-1" />
        <button onClick={() => sendText('\x03')} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-red-600 hover:bg-red-500 text-white transition-all active:scale-95 text-[11px]">Ctrl+C</button>
        <button onClick={() => sendText('\n')} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-gray-800 hover:bg-gray-700 text-gray-300 transition-all active:scale-95 text-[11px]">Enter</button>
        <div className="w-px h-5 bg-white/10 mx-1" />
        {!connected && (
          <button onClick={reconnect} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-blue-600 hover:bg-blue-500 text-white transition-all active:scale-95 text-[11px]">Reconnect</button>
        )}
        <div className="flex-1" />
        <span className={`text-[11px] px-2 py-1 rounded-md ${connected ? 'text-emerald-400 bg-emerald-500/10' : 'text-red-400 bg-red-500/10'}`}>
          {connected ? 'connected' : 'disconnected'}
        </span>
      </div>

      {/* Terminal */}
      <div className="bg-[#0d1117] rounded-xl border border-white/5 p-1.5 shadow-2xl shadow-black/50">
        <div ref={termRef} />
      </div>
    </div>
  )
}
