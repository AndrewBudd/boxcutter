'use client'

import { useEffect, useState, use } from 'react'
import type { TapegunActivity } from '@/lib/types'
import dynamic from 'next/dynamic'

const TerminalView = dynamic(() => import('@/components/terminal-view'), { ssr: false })

export default function VMDetail({ params }: { params: Promise<{ name: string }> }) {
  const { name } = use(params)
  const [vm, setVM] = useState<Record<string, unknown> | null>(null)
  const [activity, setActivity] = useState<TapegunActivity | null>(null)
  const [logs, setLogs] = useState<string>('')
  const [message, setMessage] = useState('')
  const [sendKeys, setSendKeys] = useState(false)
  const [showTerminal, setShowTerminal] = useState(false)
  const [tab, setTab] = useState<'activity' | 'logs' | 'terminal'>('activity')

  useEffect(() => {
    const poll = async () => {
      try {
        const [vmData, actData] = await Promise.all([
          fetch(`/api/vms/${name}`).then(r => r.json()),
          fetch(`/api/tapegun/activity/${name}`).then(r => r.json()),
        ])
        setVM(vmData)
        setActivity(actData)
      } catch {}
    }
    poll()
    const interval = setInterval(poll, 3000)
    return () => clearInterval(interval)
  }, [name])

  const fetchLogs = async () => {
    try {
      const data = await fetch(`/api/vms/${name}/logs?lines=200`).then(r => r.text())
      setLogs(data)
    } catch {}
  }

  useEffect(() => {
    if (tab === 'logs') fetchLogs()
  }, [tab, name])

  const sendMessage = async () => {
    if (!message.trim()) return
    try {
      await fetch(`/api/tapegun/message/${name}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ body: message, from: 'web-ui', send_keys: sendKeys }),
      })
      setMessage('')
    } catch {}
  }

  const vmData = vm as Record<string, unknown> | null
  const tailscaleIP = (vmData?.tailscale_ip as string) || ''

  return (
    <div>
      <h1 className="text-2xl font-bold mb-2">{name}</h1>
      {typeof vmData?.description === 'string' && vmData.description && (
        <p className="text-gray-400 mb-4">{vmData.description}</p>
      )}

      <div className="grid grid-cols-4 gap-4 mb-6">
        <InfoCard label="Type" value={(vmData?.type as string) || 'fc'} />
        <InfoCard label="Status" value={(vmData?.status as string) || '...'} />
        <InfoCard label="Tailscale IP" value={tailscaleIP || '-'} />
        <InfoCard label="Node" value={(vmData?.node_name as string) || '...'} />
        <InfoCard label="vCPU" value={String(vmData?.vcpu || '-')} />
        <InfoCard label="RAM" value={vmData?.ram_mib ? `${(vmData.ram_mib as number) / 1024}G` : '-'} />
        <InfoCard label="Mode" value={(vmData?.mode as string) || '-'} />
        <InfoCard label="Pending" value={String(activity?.pending_messages || 0)} />
      </div>

      {/* Tabs */}
      <div className="flex gap-1 mb-4 border-b border-gray-800">
        {(['activity', 'logs', 'terminal'] as const).map(t => (
          <button
            key={t}
            onClick={() => { setTab(t); if (t === 'terminal') setShowTerminal(true) }}
            className={`px-4 py-2 text-sm capitalize ${tab === t ? 'text-white border-b-2 border-blue-500' : 'text-gray-400 hover:text-white'}`}
          >
            {t}
          </button>
        ))}
      </div>

      {/* Activity tab */}
      {tab === 'activity' && (
        <div>
          {activity?.activity?.status && (
            <div className="mb-2 text-sm text-gray-400">
              Status: <span className="text-white">{activity.activity.status}</span>
              <span className="ml-4 text-gray-600">
                {activity.activity.timestamp && new Date(activity.activity.timestamp).toLocaleTimeString()}
              </span>
            </div>
          )}
          <div className="bg-gray-900 border border-gray-800 rounded-lg p-4 font-mono text-xs whitespace-pre-wrap max-h-[500px] overflow-y-auto text-green-400">
            {activity?.activity?.pane_content || 'No activity data. Tapegun daemon may not be running.'}
          </div>
        </div>
      )}

      {/* Logs tab */}
      {tab === 'logs' && (
        <div>
          <button onClick={fetchLogs} className="mb-2 px-3 py-1 text-xs bg-gray-800 rounded hover:bg-gray-700">
            Refresh
          </button>
          <div className="bg-gray-900 border border-gray-800 rounded-lg p-4 font-mono text-xs whitespace-pre-wrap max-h-[500px] overflow-y-auto text-gray-300">
            {logs || 'Loading...'}
          </div>
        </div>
      )}

      {/* Terminal tab */}
      {tab === 'terminal' && (
        <div>
          {tailscaleIP ? (
            showTerminal && <TerminalView vmName={name} tailscaleIP={tailscaleIP} />
          ) : (
            <div className="text-gray-400 py-8 text-center">
              No Tailscale IP — terminal requires Tailscale connectivity
            </div>
          )}
        </div>
      )}

      {/* Send message */}
      <div className="mt-6 border-t border-gray-800 pt-4">
        <h3 className="text-sm font-medium text-gray-400 mb-2">Send Message</h3>
        <div className="flex gap-2">
          <input
            type="text"
            value={message}
            onChange={e => setMessage(e.target.value)}
            onKeyDown={e => e.key === 'Enter' && sendMessage()}
            placeholder="Type a message..."
            className="flex-1 bg-gray-900 border border-gray-700 rounded px-3 py-2 text-sm focus:outline-none focus:border-blue-500"
          />
          <label className="flex items-center gap-1 text-xs text-gray-400">
            <input
              type="checkbox"
              checked={sendKeys}
              onChange={e => setSendKeys(e.target.checked)}
              className="rounded"
            />
            send_keys
          </label>
          <button
            onClick={sendMessage}
            className="px-4 py-2 bg-blue-600 rounded text-sm hover:bg-blue-500"
          >
            Send
          </button>
        </div>
      </div>
    </div>
  )
}

function InfoCard({ label, value }: { label: string; value: string }) {
  return (
    <div className="bg-gray-900 border border-gray-800 rounded p-3">
      <div className="text-xs text-gray-500">{label}</div>
      <div className="text-sm font-medium mt-0.5">{value}</div>
    </div>
  )
}
