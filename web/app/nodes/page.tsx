'use client'

import { useEffect, useState } from 'react'
import type { Node } from '@/lib/types'

export default function NodesPage() {
  const [nodes, setNodes] = useState<Node[]>([])

  useEffect(() => {
    const poll = async () => {
      try {
        const data = await fetch('/api/nodes').then(r => r.json())
        setNodes(Array.isArray(data) ? data : [])
      } catch {}
    }
    poll()
    const interval = setInterval(poll, 10000)
    return () => clearInterval(interval)
  }, [])

  const active = nodes.filter(n => n.status === 'active')
  const down = nodes.filter(n => n.status !== 'active')

  return (
    <div>
      <div className="mb-6">
        <h1 className="text-2xl font-semibold">Nodes</h1>
        <p className="text-sm text-gray-500 mt-0.5">{active.length} active, {down.length} inactive</p>
      </div>

      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
        {active.map(node => (
          <div key={node.id} className="bg-gray-900/80 border border-gray-800/80 rounded-xl transition-all duration-200 hover:border-gray-700 hover:bg-gray-900 p-5">
            <div className="flex items-center justify-between mb-3">
              <span className="font-medium">{node.tailscale_name || node.id}</span>
              <span className="flex items-center gap-1.5">
                <span className="w-2 h-2 rounded-full bg-emerald-400 shadow-sm shadow-emerald-400/50 animate-pulse" />
                <span className="text-xs text-emerald-400">active</span>
              </span>
            </div>
            <div className="space-y-2 text-xs">
              <div className="flex justify-between text-gray-500">
                <span>Bridge IP</span>
                <span className="font-mono text-gray-400">{node.bridge_ip}</span>
              </div>
              {node.tailscale_ip && (
                <div className="flex justify-between text-gray-500">
                  <span>Tailscale</span>
                  <span className="font-mono text-gray-400">{node.tailscale_ip}</span>
                </div>
              )}
              <div className="flex justify-between text-gray-500">
                <span>Last heartbeat</span>
                <span className="text-gray-400">
                  {node.last_heartbeat ? new Date(node.last_heartbeat).toLocaleTimeString() : '-'}
                </span>
              </div>
            </div>
          </div>
        ))}
      </div>

      {down.length > 0 && (
        <div className="mt-6">
          <h2 className="text-sm font-medium text-gray-500 mb-3">Inactive</h2>
          <div className="bg-gray-900/80 border border-gray-800/80 rounded-xl divide-y divide-white/[0.03]">
            {down.map(node => (
              <div key={node.id} className="px-5 py-3 flex items-center justify-between text-sm">
                <span className="text-gray-500">{node.tailscale_name || node.id}</span>
                <div className="flex items-center gap-3 text-xs text-gray-600">
                  <span className="font-mono">{node.bridge_ip}</span>
                  <span className="px-2 py-0.5 rounded-md text-[10px] font-semibold uppercase tracking-wider bg-gray-800 text-gray-500">{node.status}</span>
                </div>
              </div>
            ))}
          </div>
        </div>
      )}

      {nodes.length === 0 && (
        <div className="bg-gray-900/80 border border-gray-800/80 rounded-xl py-12 text-center text-gray-600">No nodes registered</div>
      )}
    </div>
  )
}
