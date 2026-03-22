'use client'

import { useEffect, useState } from 'react'
import Link from 'next/link'

interface VM {
  name: string
  type: string
  description?: string
  node_name: string
  tailscale_ip: string
  vcpu: number
  ram_mib: number
  status: string
}

interface Activity {
  name: string
  activity?: { status: string; timestamp: string }
  pending_messages: number
}

export default function Dashboard() {
  const [vms, setVMs] = useState<VM[]>([])
  const [activity, setActivity] = useState<Record<string, Activity>>({})
  const [loaded, setLoaded] = useState(false)

  useEffect(() => {
    const poll = async () => {
      try {
        const [vmRes, actRes] = await Promise.all([
          fetch('/api/vms').then(r => r.json()),
          fetch('/api/tapegun/activity').then(r => r.json()).catch(() => []),
        ])
        if (Array.isArray(vmRes)) setVMs(vmRes)
        const map: Record<string, Activity> = {}
        if (Array.isArray(actRes)) for (const a of actRes) map[a.name] = a
        setActivity(map)
        setLoaded(true)
      } catch {}
    }
    poll()
    const id = setInterval(poll, 5000)
    return () => clearInterval(id)
  }, [])

  const running = vms.filter(v => v.status === 'running').length
  const nodes = [...new Set(vms.map(v => v.node_name).filter(Boolean))].length

  return (
    <div>
      <h1 className="text-2xl font-bold mb-6">Dashboard</h1>

      <div className="grid grid-cols-3 gap-4 mb-8">
        <StatCard label="VMs" value={loaded ? String(vms.length) : '...'} />
        <StatCard label="Running" value={loaded ? String(running) : '...'} />
        <StatCard label="Nodes" value={loaded ? String(nodes) : '...'} />
      </div>

      <div className="bg-gray-900 rounded-lg border border-gray-800 overflow-hidden">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-gray-800 text-gray-400 text-left">
              <th className="px-4 py-3">Name</th>
              <th className="px-4 py-3">Type</th>
              <th className="px-4 py-3">IP</th>
              <th className="px-4 py-3">Node</th>
              <th className="px-4 py-3">CPU</th>
              <th className="px-4 py-3">RAM</th>
              <th className="px-4 py-3">Status</th>
              <th className="px-4 py-3">Activity</th>
            </tr>
          </thead>
          <tbody>
            {vms.map(vm => {
              const act = activity[vm.name]
              return (
                <tr key={vm.name} className="border-b border-gray-800/50 hover:bg-gray-800/50">
                  <td className="px-4 py-3">
                    <Link href={'/vms/' + vm.name} className="text-blue-400 hover:underline font-medium">
                      {vm.name}
                    </Link>
                    {vm.description && <div className="text-xs text-gray-500 mt-0.5">{vm.description}</div>}
                  </td>
                  <td className="px-4 py-3">
                    <span className={'px-2 py-0.5 rounded text-xs ' + (vm.type === 'qemu' ? 'bg-purple-900/50 text-purple-300' : 'bg-orange-900/50 text-orange-300')}>
                      {vm.type || 'fc'}
                    </span>
                  </td>
                  <td className="px-4 py-3 text-gray-400 font-mono text-xs">{vm.tailscale_ip || '-'}</td>
                  <td className="px-4 py-3 text-gray-400 text-xs">{vm.node_name}</td>
                  <td className="px-4 py-3 text-gray-400">{vm.vcpu || '-'}</td>
                  <td className="px-4 py-3 text-gray-400">{vm.ram_mib ? (vm.ram_mib / 1024).toFixed(0) + 'G' : '-'}</td>
                  <td className="px-4 py-3">
                    <span className="flex items-center gap-1.5">
                      <span className={'w-2 h-2 rounded-full ' + (vm.status === 'running' ? 'bg-green-500' : 'bg-gray-500')} />
                      <span className="text-xs text-gray-300">{vm.status}</span>
                    </span>
                  </td>
                  <td className="px-4 py-3 text-xs text-gray-400 max-w-[200px] truncate">
                    {act?.activity?.status || '-'}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
        {!loaded && <div className="text-center py-8 text-gray-500">Loading...</div>}
        {loaded && vms.length === 0 && <div className="text-center py-8 text-gray-500">No VMs found</div>}
      </div>
    </div>
  )
}

function StatCard({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-gray-900 border border-gray-800 rounded-lg p-4">
      <div className="text-gray-400 text-sm">{label}</div>
      <div className="text-3xl font-bold mt-1">{value}</div>
    </div>
  )
}
