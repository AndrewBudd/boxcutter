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

  return (
    <div>
      {/* Header */}
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-semibold">Virtual Machines</h1>
          <p className="text-sm text-gray-500 mt-0.5">Manage your ephemeral dev environments</p>
        </div>
        <button onClick={() => setShowCreate(!showCreate)}
          className="px-3 py-1.5 rounded-lg text-xs font-medium bg-emerald-600 hover:bg-emerald-500 text-white transition-all active:scale-95 px-4 py-2 text-sm">
          + New VM
        </button>
      </div>

      {/* Create Form */}
      {showCreate && (
        <div className="bg-gray-900/80 border border-gray-800/80 rounded-xl p-5 mb-6 animate-in">
          <h3 className="text-sm font-medium text-gray-300 mb-4">Create New VM</h3>
          <div className="grid grid-cols-2 md:grid-cols-5 gap-3">
            <input placeholder="Name (optional)" value={createForm.name}
              onChange={e => setCreateForm({...createForm, name: e.target.value})}
              className="bg-gray-800/50 border border-gray-700/50 rounded-lg px-3 py-2 text-sm focus:outline-none focus:border-blue-500/50 focus:ring-1 focus:ring-blue-500/20 placeholder:text-gray-600 transition-colors" />
            <select value={createForm.type}
              onChange={e => setCreateForm({...createForm, type: e.target.value})}
              className="bg-gray-800/50 border border-gray-700/50 rounded-lg px-3 py-2 text-sm focus:outline-none focus:border-blue-500/50 focus:ring-1 focus:ring-blue-500/20 placeholder:text-gray-600 transition-colors">
              <option value="firecracker">Firecracker</option>
              <option value="qemu">QEMU (Docker)</option>
            </select>
            <select value={createForm.ram}
              onChange={e => setCreateForm({...createForm, ram: e.target.value})}
              className="bg-gray-800/50 border border-gray-700/50 rounded-lg px-3 py-2 text-sm focus:outline-none focus:border-blue-500/50 focus:ring-1 focus:ring-blue-500/20 placeholder:text-gray-600 transition-colors">
              <option value="1024">1 GB RAM</option>
              <option value="2048">2 GB RAM</option>
              <option value="4096">4 GB RAM</option>
              <option value="8192">8 GB RAM</option>
            </select>
            <select value={createForm.vcpu}
              onChange={e => setCreateForm({...createForm, vcpu: e.target.value})}
              className="bg-gray-800/50 border border-gray-700/50 rounded-lg px-3 py-2 text-sm focus:outline-none focus:border-blue-500/50 focus:ring-1 focus:ring-blue-500/20 placeholder:text-gray-600 transition-colors">
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
            }} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-blue-600 hover:bg-blue-500 text-white transition-all active:scale-95 py-2 text-sm disabled:opacity-50">
              {creating ? 'Creating...' : 'Create'}
            </button>
          </div>
          <input placeholder="Description (optional)" value={createForm.desc}
            onChange={e => setCreateForm({...createForm, desc: e.target.value})}
            className="bg-gray-800/50 border border-gray-700/50 rounded-lg px-3 py-2 text-sm focus:outline-none focus:border-blue-500/50 focus:ring-1 focus:ring-blue-500/20 placeholder:text-gray-600 transition-colors mt-3 w-full" />
        </div>
      )}

      {/* Stats */}
      <div className="grid grid-cols-3 gap-3 md:gap-4 mb-8">
        <StatCard label="Total VMs" value={loaded ? String(vms.length) : '-'} color="blue" />
        <StatCard label="Running" value={loaded ? String(running) : '-'} color="emerald" />
        <StatCard label="Nodes" value={loaded ? String(nodeCount) : '-'} color="purple" />
      </div>

      {/* Desktop table */}
      <div className="hidden md:block card overflow-hidden">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-white/5 text-gray-500 text-left text-xs uppercase tracking-wider">
              <th className="px-5 py-3 font-medium">Name</th>
              <th className="px-5 py-3 font-medium">Type</th>
              <th className="px-5 py-3 font-medium">IP Address</th>
              <th className="px-5 py-3 font-medium">Node</th>
              <th className="px-5 py-3 font-medium">Resources</th>
              <th className="px-5 py-3 font-medium">Status</th>
              <th className="px-5 py-3 font-medium">Activity</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-white/[0.03]">
            {vms.map(vm => {
              const act = activity[vm.name]
              return (
                <tr key={vm.name} className="hover:bg-white/[0.02] transition-colors">
                  <td className="px-5 py-3.5">
                    <Link href={'/vms/' + vm.name} className="text-blue-400 hover:text-blue-300 font-medium transition-colors">{vm.name}</Link>
                    {vm.description && <div className="text-xs text-gray-600 mt-0.5 max-w-[200px] truncate">{vm.description}</div>}
                  </td>
                  <td className="px-5 py-3.5"><TypeBadge type={vm.type} /></td>
                  <td className="px-5 py-3.5 font-mono text-xs text-gray-400">{vm.tailscale_ip || <span className="text-gray-600">pending</span>}</td>
                  <td className="px-5 py-3.5 text-gray-500 text-xs">{vm.node_name?.replace('boxcutter-', '')}</td>
                  <td className="px-5 py-3.5 text-gray-500 text-xs">{vm.vcpu}c / {vm.ram_mib ? (vm.ram_mib / 1024).toFixed(0) : '-'}G</td>
                  <td className="px-5 py-3.5"><StatusPill status={vm.status} /></td>
                  <td className="px-5 py-3.5 text-xs text-gray-600 max-w-[180px] truncate">{act?.activity?.status || ''}</td>
                </tr>
              )
            })}
          </tbody>
        </table>
        {!loaded && <div className="text-center py-12 text-gray-600">Loading...</div>}
        {loaded && vms.length === 0 && <div className="text-center py-12 text-gray-600">No VMs. Click <span className="text-emerald-400">+ New VM</span> to create one.</div>}
      </div>

      {/* Mobile cards */}
      <div className="md:hidden flex flex-col gap-3">
        {!loaded && <div className="text-center py-12 text-gray-600">Loading...</div>}
        {vms.map(vm => {
          const act = activity[vm.name]
          return (
            <Link key={vm.name} href={'/vms/' + vm.name} className="bg-gray-900/80 border border-gray-800/80 rounded-xl transition-all duration-200 hover:border-gray-700 hover:bg-gray-900 p-4 block">
              <div className="flex items-center justify-between mb-2">
                <span className="text-blue-400 font-medium">{vm.name}</span>
                <div className="flex items-center gap-2">
                  <TypeBadge type={vm.type} />
                  <StatusPill status={vm.status} />
                </div>
              </div>
              {vm.description && <div className="text-xs text-gray-600 mb-2">{vm.description}</div>}
              <div className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs text-gray-500">
                <div>IP: <span className="font-mono text-gray-400">{vm.tailscale_ip || '-'}</span></div>
                <div>{vm.node_name?.replace('boxcutter-', '')}</div>
                <div>{vm.vcpu}c / {vm.ram_mib ? (vm.ram_mib / 1024).toFixed(0) + 'G' : '-'}</div>
                <div className="truncate">{act?.activity?.status || ''}</div>
              </div>
            </Link>
          )
        })}
      </div>
    </div>
  )
}

function StatCard({ label, value, color }: { label: string; value: string; color: string }) {
  const gradients: Record<string, string> = {
    blue: 'from-blue-500/10 to-blue-500/5 border-blue-500/10',
    emerald: 'from-emerald-500/10 to-emerald-500/5 border-emerald-500/10',
    purple: 'from-purple-500/10 to-purple-500/5 border-purple-500/10',
  }
  const textColors: Record<string, string> = {
    blue: 'text-blue-400',
    emerald: 'text-emerald-400',
    purple: 'text-purple-400',
  }
  return (
    <div className={`rounded-xl border bg-gradient-to-br p-4 md:p-5 ${gradients[color]}`}>
      <div className="text-xs text-gray-500 font-medium uppercase tracking-wider">{label}</div>
      <div className={`text-3xl md:text-4xl font-bold mt-1 ${textColors[color]}`}>{value}</div>
    </div>
  )
}

function TypeBadge({ type }: { type: string }) {
  const isQemu = type === 'qemu'
  return (
    <span className={`badge ${isQemu ? 'bg-purple-500/15 text-purple-400 border border-purple-500/20' : 'bg-amber-500/15 text-amber-400 border border-amber-500/20'}`}>
      {isQemu ? 'qemu' : 'fc'}
    </span>
  )
}

function StatusPill({ status }: { status: string }) {
  const isRunning = status === 'running'
  return (
    <span className="flex items-center gap-1.5">
      <span className={`w-1.5 h-1.5 rounded-full ${isRunning ? 'bg-emerald-400 shadow-sm shadow-emerald-400/50' : 'bg-gray-500'}`} />
      <span className={`text-xs ${isRunning ? 'text-emerald-400' : 'text-gray-500'}`}>{status}</span>
    </span>
  )
}
