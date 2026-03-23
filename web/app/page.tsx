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
  const [nodeCount, setNodeCount] = useState(0)
  const [showCreate, setShowCreate] = useState(false)
  const [creating, setCreating] = useState(false)
  const [createForm, setCreateForm] = useState({ name: '', type: 'firecracker', ram: '2048', vcpu: '2', desc: '' })

  useEffect(() => {
    const poll = async () => {
      try {
        const [vmRes, actRes, nodeRes] = await Promise.all([
          fetch('/api/vms').then(r => r.json()),
          fetch('/api/tapegun/activity').then(r => r.json()).catch(() => []),
          fetch('/api/nodes').then(r => r.json()).catch(() => []),
        ])
        if (Array.isArray(vmRes)) setVMs(vmRes)
        if (Array.isArray(nodeRes)) setNodeCount(nodeRes.filter((n: {status: string}) => n.status === 'active').length)
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
  // nodeCount is fetched from /api/nodes (active nodes only)

  return (
    <div>
      <div className="flex items-center justify-between mb-4 md:mb-6">
        <h1 className="text-xl md:text-2xl font-bold">Dashboard</h1>
        <button onClick={() => setShowCreate(!showCreate)}
          className="px-4 py-2 bg-green-700 hover:bg-green-600 rounded text-sm font-medium">
          + New VM
        </button>
      </div>

      {showCreate && (
        <div className="bg-gray-900 border border-gray-800 rounded-lg p-4 mb-6">
          <h3 className="text-sm font-medium text-gray-300 mb-3">Create New VM</h3>
          <div className="grid grid-cols-2 md:grid-cols-5 gap-3">
            <input placeholder="Name (optional)" value={createForm.name}
              onChange={e => setCreateForm({...createForm, name: e.target.value})}
              className="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm focus:outline-none focus:border-blue-500" />
            <select value={createForm.type}
              onChange={e => setCreateForm({...createForm, type: e.target.value})}
              className="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm focus:outline-none focus:border-blue-500">
              <option value="firecracker">Firecracker</option>
              <option value="qemu">QEMU (Docker)</option>
            </select>
            <select value={createForm.ram}
              onChange={e => setCreateForm({...createForm, ram: e.target.value})}
              className="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm focus:outline-none focus:border-blue-500">
              <option value="1024">1 GB RAM</option>
              <option value="2048">2 GB RAM</option>
              <option value="4096">4 GB RAM</option>
              <option value="8192">8 GB RAM</option>
            </select>
            <select value={createForm.vcpu}
              onChange={e => setCreateForm({...createForm, vcpu: e.target.value})}
              className="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm focus:outline-none focus:border-blue-500">
              <option value="1">1 vCPU</option>
              <option value="2">2 vCPU</option>
              <option value="4">4 vCPU</option>
            </select>
            <button disabled={creating} onClick={async () => {
              setCreating(true)
              try {
                const body: Record<string, unknown> = {
                  type: createForm.type,
                  ram_mib: parseInt(createForm.ram),
                  vcpu: parseInt(createForm.vcpu),
                }
                if (createForm.name) body.name = createForm.name
                if (createForm.desc) body.description = createForm.desc
                await fetch('/api/vms/create', {
                  method: 'POST',
                  headers: { 'Content-Type': 'application/json' },
                  body: JSON.stringify(body),
                })
                setShowCreate(false)
                setCreateForm({ name: '', type: 'firecracker', ram: '2048', vcpu: '2', desc: '' })
              } catch {}
              setCreating(false)
            }} className="px-4 py-2 bg-blue-600 hover:bg-blue-500 rounded text-sm font-medium disabled:opacity-50">
              {creating ? 'Creating...' : 'Create'}
            </button>
          </div>
          <input placeholder="Description (optional)" value={createForm.desc}
            onChange={e => setCreateForm({...createForm, desc: e.target.value})}
            className="mt-3 w-full bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm focus:outline-none focus:border-blue-500" />
        </div>
      )}

      <div className="grid grid-cols-3 gap-2 md:gap-4 mb-6 md:mb-8">
        <StatCard label="VMs" value={loaded ? String(vms.length) : '...'} />
        <StatCard label="Running" value={loaded ? String(running) : '...'} />
        <StatCard label="Nodes" value={loaded ? String(nodeCount) : '...'} />
      </div>

      {/* Desktop table */}
      <div className="hidden md:block bg-gray-900 rounded-lg border border-gray-800 overflow-hidden">
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
                    <Link href={'/vms/' + vm.name} className="text-blue-400 hover:underline font-medium">{vm.name}</Link>
                    {vm.description && <div className="text-xs text-gray-500 mt-0.5">{vm.description}</div>}
                  </td>
                  <td className="px-4 py-3"><TypeBadge type={vm.type} /></td>
                  <td className="px-4 py-3 text-gray-400 font-mono text-xs">{vm.tailscale_ip || '-'}</td>
                  <td className="px-4 py-3 text-gray-400 text-xs">{vm.node_name}</td>
                  <td className="px-4 py-3 text-gray-400">{vm.vcpu || '-'}</td>
                  <td className="px-4 py-3 text-gray-400">{vm.ram_mib ? (vm.ram_mib / 1024).toFixed(0) + 'G' : '-'}</td>
                  <td className="px-4 py-3"><StatusDot status={vm.status} /></td>
                  <td className="px-4 py-3 text-xs text-gray-400 max-w-[200px] truncate">{act?.activity?.status || '-'}</td>
                </tr>
              )
            })}
          </tbody>
        </table>
        {!loaded && <div className="text-center py-8 text-gray-500">Loading...</div>}
        {loaded && vms.length === 0 && <div className="text-center py-8 text-gray-500">No VMs found</div>}
      </div>

      {/* Mobile cards */}
      <div className="md:hidden flex flex-col gap-3">
        {!loaded && <div className="text-center py-8 text-gray-500">Loading...</div>}
        {vms.map(vm => {
          const act = activity[vm.name]
          return (
            <Link key={vm.name} href={'/vms/' + vm.name}
              className="block bg-gray-900 border border-gray-800 rounded-lg p-4 active:bg-gray-800">
              <div className="flex items-center justify-between mb-2">
                <span className="text-blue-400 font-medium">{vm.name}</span>
                <div className="flex items-center gap-2">
                  <TypeBadge type={vm.type} />
                  <StatusDot status={vm.status} />
                </div>
              </div>
              {vm.description && <div className="text-xs text-gray-500 mb-2">{vm.description}</div>}
              <div className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs text-gray-400">
                <div>IP: <span className="font-mono">{vm.tailscale_ip || '-'}</span></div>
                <div>Node: {vm.node_name?.replace('boxcutter-', '')}</div>
                <div>CPU: {vm.vcpu || '-'} / RAM: {vm.ram_mib ? (vm.ram_mib / 1024).toFixed(0) + 'G' : '-'}</div>
                <div className="truncate">Act: {act?.activity?.status || '-'}</div>
              </div>
            </Link>
          )
        })}
      </div>
    </div>
  )
}

function StatCard({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-gray-900 border border-gray-800 rounded-lg p-3 md:p-4">
      <div className="text-gray-400 text-xs md:text-sm">{label}</div>
      <div className="text-2xl md:text-3xl font-bold mt-1">{value}</div>
    </div>
  )
}

function TypeBadge({ type }: { type: string }) {
  return (
    <span className={'px-2 py-0.5 rounded text-xs ' + (type === 'qemu' ? 'bg-purple-900/50 text-purple-300' : 'bg-orange-900/50 text-orange-300')}>
      {type || 'fc'}
    </span>
  )
}

function StatusDot({ status }: { status: string }) {
  return (
    <span className="flex items-center gap-1.5">
      <span className={'w-2 h-2 rounded-full ' + (status === 'running' ? 'bg-green-500' : 'bg-gray-500')} />
      <span className="text-xs text-gray-300">{status}</span>
    </span>
  )
}
