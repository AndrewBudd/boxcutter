'use client'

import { useEffect, useState } from 'react'
import type { Node } from '@/lib/types'

export default function NodesPage() {
  const [nodes, setNodes] = useState<Node[]>([])

  useEffect(() => {
    const poll = async () => {
      try {
        const data = await fetch('/api/nodes').then(r => r.json())
        setNodes(Array.isArray(data) ? data.filter((n: Node) => n.status === 'active') : [])
      } catch {}
    }
    poll()
    const interval = setInterval(poll, 10000)
    return () => clearInterval(interval)
  }, [])

  return (
    <div>
      <h1 className="text-2xl font-bold mb-6">Nodes</h1>
      <div className="bg-gray-900 rounded-lg border border-gray-800 overflow-hidden">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-gray-800 text-gray-400 text-left">
              <th className="px-4 py-3">ID</th>
              <th className="px-4 py-3">Name</th>
              <th className="px-4 py-3">Bridge IP</th>
              <th className="px-4 py-3">Status</th>
              <th className="px-4 py-3">Last Heartbeat</th>
            </tr>
          </thead>
          <tbody>
            {nodes.map(node => (
              <tr key={node.id} className="border-b border-gray-800/50 hover:bg-gray-800/50">
                <td className="px-4 py-3 font-medium">{node.id}</td>
                <td className="px-4 py-3 text-gray-400">{node.tailscale_name}</td>
                <td className="px-4 py-3 text-gray-400 font-mono text-xs">{node.bridge_ip}</td>
                <td className="px-4 py-3">
                  <span className={`px-2 py-0.5 rounded text-xs ${node.status === 'active' ? 'bg-green-900/50 text-green-300' : 'bg-gray-700 text-gray-400'}`}>
                    {node.status}
                  </span>
                </td>
                <td className="px-4 py-3 text-gray-500 text-xs">
                  {node.last_heartbeat ? new Date(node.last_heartbeat).toLocaleString() : '-'}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {nodes.length === 0 && (
          <div className="text-center py-8 text-gray-500">No active nodes</div>
        )}
      </div>
    </div>
  )
}
