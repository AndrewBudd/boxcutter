'use client'

import { useEffect, useState, use } from 'react'
import dynamic from 'next/dynamic'
import Link from 'next/link'

const TerminalView = dynamic(() => import('@/components/terminal-view'), { ssr: false })

export default function VMDetail({ params }: { params: Promise<{ name: string }> }) {
  const { name } = use(params)
  const [vm, setVM] = useState<Record<string, unknown> | null>(null)
  const [activity, setActivity] = useState<Record<string, unknown> | null>(null)
  const [logs, setLogs] = useState('')
  const [tab, setTab] = useState<'activity' | 'logs' | 'terminal'>('activity')
  const [message, setMessage] = useState('')
  const [sendKeys, setSendKeys] = useState(false)
  const [msgStatus, setMsgStatus] = useState('')
  const [actionMsg, setActionMsg] = useState('')
  const [copyName, setCopyName] = useState('')
  const [showCopy, setShowCopy] = useState(false)
  const [confirmDestroy, setConfirmDestroy] = useState(false)

  const vmAction = async (action: string, body?: object) => {
    setActionMsg(action + '...')
    try {
      const opts: RequestInit = { method: 'POST' }
      if (body) {
        opts.headers = { 'Content-Type': 'application/json' }
        opts.body = JSON.stringify(body)
      }
      await fetch('/api/vms/' + name + '/' + action, opts)
      setActionMsg(action + ' done')
      setTimeout(() => setActionMsg(''), 3000)
    } catch (e) {
      setActionMsg('Error: ' + e)
    }
  }

  useEffect(() => {
    const poll = async () => {
      try { setVM(await fetch('/api/vms/' + name).then(r => r.json())) } catch {}
      try { setActivity(await fetch('/api/tapegun/activity/' + name).then(r => r.json())) } catch {}
    }
    poll()
    const id = setInterval(poll, 3000)
    return () => clearInterval(id)
  }, [name])

  useEffect(() => {
    if (tab === 'logs') {
      fetch('/api/vms/' + name + '/logs?lines=200').then(r => r.text()).then(setLogs).catch(() => {})
    }
  }, [tab, name])

  const sendMessage = async () => {
    if (!message.trim()) return
    try {
      await fetch('/api/tapegun/message/' + name, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ body: message, from: 'web-ui', send_keys: sendKeys }),
      })
      setMsgStatus('Sent!')
      setMessage('')
      setTimeout(() => setMsgStatus(''), 3000)
    } catch (e) {
      setMsgStatus('Error: ' + e)
    }
  }

  const actRaw = activity as Record<string, unknown> | null
  const actData = actRaw?.activity as Record<string, string> | undefined
  const tsIP = vm?.tailscale_ip as string || ''
  const isRunning = vm?.status === 'running'
  const isStopped = vm?.status === 'stopped'
  const vmType = String(vm?.type || 'fc')

  return (
    <div>
      {/* Breadcrumb */}
      <div className="flex items-center gap-2 text-sm text-gray-500 mb-4">
        <Link href="/" className="hover:text-gray-300 transition-colors">VMs</Link>
        <span>/</span>
        <span className="text-gray-300">{name}</span>
      </div>

      {/* Header */}
      <div className="flex flex-col md:flex-row md:items-center gap-3 mb-6">
        <div className="flex-1">
          <div className="flex items-center gap-3">
            <h1 className="text-2xl font-semibold">{name}</h1>
            <span className={`badge ${vmType === 'qemu' ? 'bg-purple-500/15 text-purple-400 border border-purple-500/20' : 'bg-amber-500/15 text-amber-400 border border-amber-500/20'}`}>
              {vmType === 'qemu' ? 'QEMU' : 'FC'}
            </span>
            <span className="flex items-center gap-1.5">
              <span className={`w-2 h-2 rounded-full ${isRunning ? 'bg-emerald-400 shadow-sm shadow-emerald-400/50 animate-pulse' : 'bg-gray-500'}`} />
              <span className={`text-sm ${isRunning ? 'text-emerald-400' : 'text-gray-500'}`}>{String(vm?.status || '...')}</span>
            </span>
          </div>
          {typeof vm?.description === 'string' && vm.description && (
            <p className="text-sm text-gray-500 mt-1">{vm.description}</p>
          )}
        </div>

        {/* Action buttons */}
        <div className="flex items-center gap-2 flex-wrap">
          {isRunning && (
            <>
              <button onClick={() => vmAction('stop')} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-amber-600 hover:bg-amber-500 text-white transition-all active:scale-95">Stop</button>
              <button onClick={() => vmAction('restart')} className="px-3 py-1.5 rounded-lg text-xs font-medium transition-all active:scale-95 bg-orange-600 hover:bg-orange-500 text-white shadow-sm shadow-orange-900/30">Restart</button>
            </>
          )}
          {isStopped && <button onClick={() => vmAction('start')} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-emerald-600 hover:bg-emerald-500 text-white transition-all active:scale-95">Start</button>}
          <button onClick={() => setShowCopy(!showCopy)} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-blue-600 hover:bg-blue-500 text-white transition-all active:scale-95">Copy</button>
          {!confirmDestroy ? (
            <button onClick={() => setConfirmDestroy(true)} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-red-600 hover:bg-red-500 text-white transition-all active:scale-95">Destroy</button>
          ) : (
            <span className="flex gap-1">
              <button onClick={() => { vmAction('destroy'); setConfirmDestroy(false) }} className="px-3 py-1.5 rounded-lg text-xs font-medium transition-all active:scale-95 bg-red-500 hover:bg-red-400 text-white">Confirm</button>
              <button onClick={() => setConfirmDestroy(false)} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-gray-800 hover:bg-gray-700 text-gray-300 transition-all active:scale-95">Cancel</button>
            </span>
          )}
          {actionMsg && <span className="text-xs text-gray-500 animate-pulse">{actionMsg}</span>}
        </div>
      </div>

      {/* Copy form */}
      {showCopy && (
        <div className="bg-gray-900/80 border border-gray-800/80 rounded-xl p-4 mb-6 flex gap-2">
          <input type="text" value={copyName} onChange={e => setCopyName(e.target.value)}
            placeholder="New VM name..."
            className="bg-gray-800/50 border border-gray-700/50 rounded-lg px-3 py-2 text-sm focus:outline-none focus:border-blue-500/50 focus:ring-1 focus:ring-blue-500/20 placeholder:text-gray-600 transition-colors w-60" />
          <button onClick={() => { if (copyName.trim()) { vmAction('copy', { name: copyName }); setShowCopy(false); setCopyName('') } }}
            className="px-3 py-1.5 rounded-lg text-xs font-medium bg-blue-600 hover:bg-blue-500 text-white transition-all active:scale-95">Create Copy</button>
          <button onClick={() => setShowCopy(false)} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-gray-800 hover:bg-gray-700 text-gray-300 transition-all active:scale-95">Cancel</button>
        </div>
      )}

      {/* Info grid */}
      <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mb-6">
        <InfoCard label="Tailscale IP" value={tsIP || 'pending'} mono />
        <InfoCard label="Node" value={String(vm?.node_name || '...').replace('boxcutter-', '')} />
        <InfoCard label="Resources" value={`${vm?.vcpu || '-'} vCPU / ${vm?.ram_mib ? (Number(vm.ram_mib) / 1024) + 'G' : '-'} RAM`} />
        <InfoCard label="Mode" value={String(vm?.mode || '-')} />
      </div>

      {/* Tabs */}
      <div className="flex gap-1 mb-4 border-b border-white/5">
        {(['activity', 'logs', 'terminal'] as const).map(t => (
          <button key={t} onClick={() => setTab(t)}
            className={'px-4 py-2.5 text-sm capitalize transition-all duration-150 ' +
              (tab === t
                ? 'text-white border-b-2 border-blue-500 font-medium'
                : 'text-gray-500 hover:text-gray-300 border-b-2 border-transparent')}>
            {t}
          </button>
        ))}
      </div>

      {/* Tab content */}
      {tab === 'activity' && (
        <div>
          {actData && actData.status && (
            <div className="flex items-center gap-3 mb-3 text-sm">
              <span className="text-gray-500">Status:</span>
              <span className="text-white font-medium">{String(actData.status)}</span>
              {actData?.timestamp && <span className="text-gray-600 text-xs">{new Date(String(actData.timestamp)).toLocaleTimeString()}</span>}
            </div>
          )}
          <div className="bg-[#0d1117] border border-gray-800/80 rounded-xl p-4 font-mono text-xs whitespace-pre-wrap max-h-[500px] overflow-y-auto text-green-400 leading-relaxed"
               style={{ fontFamily: "'JetBrains Mono', monospace" }}>
            {actData?.pane_content ? String(actData.pane_content) : <span className="text-gray-600">No activity data. Tapegun daemon may not be running in this VM.</span>}
          </div>
        </div>
      )}

      {tab === 'logs' && (
        <div>
          <button onClick={() => fetch('/api/vms/' + name + '/logs?lines=200').then(r => r.text()).then(setLogs)}
            className="px-3 py-1.5 rounded-lg text-xs font-medium bg-gray-800 hover:bg-gray-700 text-gray-300 transition-all active:scale-95 mb-3">Refresh Logs</button>
          <div className="bg-[#0d1117] border border-gray-800/80 rounded-xl p-4 font-mono text-xs whitespace-pre-wrap max-h-[500px] overflow-y-auto text-gray-400 leading-relaxed"
               style={{ fontFamily: "'JetBrains Mono', monospace" }}>
            {logs || <span className="text-gray-600">Loading...</span>}
          </div>
        </div>
      )}

      {tab === 'terminal' && (
        <div>
          {tsIP ? <TerminalView vmName={name} tailscaleIP={tsIP} /> : (
            <div className="bg-gray-900/80 border border-gray-800/80 rounded-xl py-12 text-center text-gray-500">No Tailscale IP yet. Terminal requires network connectivity.</div>
          )}
        </div>
      )}

      {/* Tapegun message */}
      {tab !== 'terminal' && (
        <div className="mt-8 pt-6 border-t border-white/5">
          <h3 className="text-xs font-medium text-gray-500 uppercase tracking-wider mb-3">Send Message</h3>
          <div className="flex gap-2">
            <input type="text" value={message} onChange={e => setMessage(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && sendMessage()}
              placeholder="Type a message to send to this VM..."
              className="bg-gray-800/50 border border-gray-700/50 rounded-lg px-3 py-2 text-sm focus:outline-none focus:border-blue-500/50 focus:ring-1 focus:ring-blue-500/20 placeholder:text-gray-600 transition-colors flex-1" />
            <label className="flex items-center gap-1.5 text-xs text-gray-500 select-none cursor-pointer">
              <input type="checkbox" checked={sendKeys} onChange={e => setSendKeys(e.target.checked)}
                className="rounded border-gray-600" /> keys
            </label>
            <button onClick={sendMessage} className="px-3 py-1.5 rounded-lg text-xs font-medium bg-blue-600 hover:bg-blue-500 text-white transition-all active:scale-95 px-5">Send</button>
          </div>
          {msgStatus && <div className="text-xs text-emerald-500 mt-2">{msgStatus}</div>}
        </div>
      )}
    </div>
  )
}

function InfoCard({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="bg-gray-900/80 border border-gray-800/80 rounded-xl p-3.5">
      <div className="text-[10px] text-gray-600 uppercase tracking-wider font-medium">{label}</div>
      <div className={`text-sm font-medium mt-1 ${mono ? 'font-mono text-gray-300' : 'text-gray-200'}`}>{value}</div>
    </div>
  )
}
