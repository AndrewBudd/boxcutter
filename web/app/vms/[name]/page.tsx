'use client'

import { useEffect, useState, use } from 'react'
import dynamic from 'next/dynamic'

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

  return (
    <div>
      <h1 className="text-2xl font-bold mb-2">{name}</h1>
      {typeof vm?.description === 'string' && vm.description && (
        <p className="text-gray-400 mb-4">{vm.description}</p>
      )}

      <div className="grid grid-cols-4 gap-4 mb-6">
        <Info label="Type" value={String(vm?.type || 'fc')} />
        <Info label="Status" value={String(vm?.status || '...')} />
        <Info label="Tailscale IP" value={tsIP || '-'} />
        <Info label="Node" value={String(vm?.node_name || '...')} />
        <Info label="vCPU" value={String(vm?.vcpu || '-')} />
        <Info label="RAM" value={vm?.ram_mib ? (Number(vm.ram_mib) / 1024) + 'G' : '-'} />
        <Info label="Mode" value={String(vm?.mode || '-')} />
        <Info label="Pending" value={String(actRaw?.pending_messages || 0)} />
      </div>

      <div className="flex gap-1 mb-4 border-b border-gray-800">
        {(['activity', 'logs', 'terminal'] as const).map(t => (
          <button key={t} onClick={() => setTab(t)}
            className={'px-4 py-2 text-sm capitalize ' + (tab === t ? 'text-white border-b-2 border-blue-500' : 'text-gray-400 hover:text-white')}>
            {t}
          </button>
        ))}
      </div>

      {tab === 'activity' && (
        <div>
          {actData && actData.status && (
            <div className="mb-2 text-sm text-gray-400">
              Status: <span className="text-white">{String(actData.status)}</span>
              {actData?.timestamp ? <span className="ml-4 text-gray-600">{new Date(String(actData?.timestamp)).toLocaleTimeString()}</span> : null}
            </div>
          )}
          <div className="bg-[#0d1117] border border-gray-800 rounded-lg p-4 font-mono text-xs whitespace-pre-wrap max-h-[500px] overflow-y-auto text-green-400">
            {actData?.pane_content ? String(actData.pane_content) : 'No activity data. Tapegun daemon may not be running.'}
          </div>
        </div>
      )}

      {tab === 'logs' && (
        <div>
          <button onClick={() => fetch('/api/vms/' + name + '/logs?lines=200').then(r => r.text()).then(setLogs)}
            className="mb-2 px-3 py-1 text-xs bg-gray-800 rounded hover:bg-gray-700">Refresh</button>
          <div className="bg-gray-900 border border-gray-800 rounded-lg p-4 font-mono text-xs whitespace-pre-wrap max-h-[500px] overflow-y-auto text-gray-300">
            {logs || 'Loading...'}
          </div>
        </div>
      )}

      {tab === 'terminal' && (
        <div>
          {tsIP ? <TerminalView vmName={name} tailscaleIP={tsIP} /> : (
            <div className="text-gray-400 py-8 text-center">No Tailscale IP — terminal requires connectivity</div>
          )}
        </div>
      )}

      <div className="mt-6 border-t border-gray-800 pt-4">
        <h3 className="text-sm font-medium text-gray-400 mb-2">Send Message</h3>
        <div className="flex gap-2">
          <input type="text" value={message} onChange={e => setMessage(e.target.value)}
            onKeyDown={e => e.key === 'Enter' && sendMessage()}
            placeholder="Type a message..."
            className="flex-1 bg-gray-900 border border-gray-700 rounded px-3 py-2 text-sm focus:outline-none focus:border-blue-500" />
          <label className="flex items-center gap-1 text-xs text-gray-400">
            <input type="checkbox" checked={sendKeys} onChange={e => setSendKeys(e.target.checked)} /> send_keys
          </label>
          <button onClick={sendMessage} className="px-4 py-2 bg-blue-600 rounded text-sm hover:bg-blue-500">Send</button>
        </div>
        {msgStatus && <div className="text-xs text-gray-500 mt-1">{msgStatus}</div>}
      </div>
    </div>
  )
}

function Info({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-gray-900 border border-gray-800 rounded p-3">
      <div className="text-xs text-gray-500">{label}</div>
      <div className="text-sm font-medium mt-0.5">{value}</div>
    </div>
  )
}
