'use client'

import { useEffect, useRef, useState } from 'react'
import Link from 'next/link'
import type { TapegunActivity, AlertEvent } from '@/lib/types'

type StatusColor = 'green' | 'yellow' | 'red' | 'gray'

function statusColor(act: TapegunActivity): StatusColor {
  if (act.vm_status !== 'running') return 'gray'
  const status = act.activity?.status
  if (status === 'active') return 'green'
  if (status === 'idle') return 'yellow'
  if (status === 'stopped' || status === 'error') return 'red'
  return 'gray'
}

function statusDot(color: StatusColor) {
  const cls: Record<StatusColor, string> = {
    green: 'bg-emerald-400',
    yellow: 'bg-yellow-400',
    red: 'bg-red-400',
    gray: 'bg-gray-600',
  }
  return <span className={`w-2 h-2 rounded-full inline-block ${cls[color]}`} />
}

function alertIcon(type: string) {
  switch (type) {
    case 'crash': return '\u{1F534}' // red circle
    case 'error': return '\u{1F534}'
    case 'unhealthy': return '\u{1F534}'
    case 'idle': return '\u{1F7E1}' // yellow circle
    case 'degraded': return '\u{1F7E1}'
    case 'unresponsive': return '\u{1F7E1}'
    case 'recovery': return '\u{1F7E2}' // green circle
    default: return '\u26AA' // white circle
  }
}

function groupByTeam(activities: TapegunActivity[]): Map<string, TapegunActivity[]> {
  const groups = new Map<string, TapegunActivity[]>()
  for (const act of activities) {
    const parts = act.name.split('-')
    let team = ''
    if (parts.length >= 3) {
      const last = parts[parts.length - 1]
      if (/^\d+$/.test(last)) {
        team = parts.slice(0, -2).join('-')
      }
    }
    const key = team || '_ungrouped'
    if (!groups.has(key)) groups.set(key, [])
    groups.get(key)!.push(act)
  }
  return groups
}

export default function ActivityPage() {
  const [activities, setActivities] = useState<TapegunActivity[]>([])
  const [alerts, setAlerts] = useState<AlertEvent[]>([])
  const paneRefs = useRef<Map<string, HTMLDivElement>>(new Map())

  // Poll activity
  useEffect(() => {
    const poll = async () => {
      try {
        const data = await fetch('/api/tapegun/activity').then(r => r.json())
        const sorted = Array.isArray(data)
          ? [...data].sort((a, b) => a.name.localeCompare(b.name))
          : []
        setActivities(sorted)
      } catch {}
    }
    poll()
    const interval = setInterval(poll, 5000)
    return () => clearInterval(interval)
  }, [])

  // Fetch initial alerts + SSE stream
  useEffect(() => {
    fetch('/api/alerts').then(r => r.json()).then(data => {
      if (Array.isArray(data)) setAlerts(data.slice(-50))
    }).catch(() => {})

    const apiBase = process.env.NEXT_PUBLIC_ORCHESTRATOR_API || ''
    const es = new EventSource(`${apiBase}/api/alerts/stream`)
    es.addEventListener('alert', (e) => {
      try {
        const evt: AlertEvent = JSON.parse(e.data)
        setAlerts(prev => [...prev.slice(-49), evt])
      } catch {}
    })
    es.onerror = () => {
      // SSE failed, fall back to polling
      es.close()
      const pollAlerts = setInterval(async () => {
        try {
          const data = await fetch('/api/alerts').then(r => r.json())
          if (Array.isArray(data)) setAlerts(data.slice(-50))
        } catch {}
      }, 15000)
      return () => clearInterval(pollAlerts)
    }
    return () => es.close()
  }, [])

  // Auto-scroll panes
  useEffect(() => {
    paneRefs.current.forEach(el => {
      el.scrollTop = el.scrollHeight
    })
  }, [activities])

  // Aggregate stats
  const stats = { active: 0, idle: 0, crashed: 0, error: 0, total: activities.length }
  for (const act of activities) {
    const c = statusColor(act)
    if (c === 'green') stats.active++
    else if (c === 'yellow') stats.idle++
    else if (c === 'red') stats.crashed++
    else stats.error++
  }

  const teams = groupByTeam(activities)

  return (
    <div className="flex gap-4">
      {/* Main content */}
      <div className="flex-1 min-w-0">
        <div className="mb-6">
          <h1 className="text-2xl font-semibold">Activity Dashboard</h1>
          <p className="text-sm text-gray-500 mt-0.5">Live agent health and activity</p>
        </div>

        {/* Aggregate stats bar */}
        <div className="flex gap-3 mb-6 text-sm">
          <div className="bg-gray-900/80 border border-gray-800/80 rounded-lg px-4 py-2 flex items-center gap-2">
            <span className="text-gray-400">{stats.total} agents</span>
          </div>
          <div className="bg-gray-900/80 border border-gray-800/80 rounded-lg px-4 py-2 flex items-center gap-2">
            {statusDot('green')} <span className="text-emerald-400">{stats.active} active</span>
          </div>
          <div className="bg-gray-900/80 border border-gray-800/80 rounded-lg px-4 py-2 flex items-center gap-2">
            {statusDot('yellow')} <span className="text-yellow-400">{stats.idle} idle</span>
          </div>
          {stats.crashed > 0 && (
            <div className="bg-red-900/20 border border-red-800/40 rounded-lg px-4 py-2 flex items-center gap-2">
              {statusDot('red')} <span className="text-red-400">{stats.crashed} crashed</span>
            </div>
          )}
        </div>

        {/* Team-grouped view */}
        {Array.from(teams.entries()).map(([teamName, teamActs]) => (
          <div key={teamName} className="mb-8">
            {teamName !== '_ungrouped' && (
              <h2 className="text-lg font-medium text-gray-300 mb-3">{teamName}</h2>
            )}
            {teamName === '_ungrouped' && teams.size > 1 && (
              <h2 className="text-lg font-medium text-gray-500 mb-3">Other VMs</h2>
            )}
            <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
              {teamActs.map(act => {
                const color = statusColor(act)
                return (
                  <div key={act.name} className="bg-gray-900/80 border border-gray-800/80 rounded-xl overflow-hidden">
                    <div className="flex items-center justify-between px-4 py-2.5 border-b border-white/5 bg-white/[0.02]">
                      <div className="flex items-center gap-2">
                        {statusDot(color)}
                        <Link href={`/vms/${act.name}`} className="font-medium text-blue-400 hover:text-blue-300 transition-colors">
                          {act.name}
                        </Link>
                        <span className="text-xs text-gray-600">{act.node_name?.replace('boxcutter-', '')}</span>
                      </div>
                      <div className="flex items-center gap-2 text-xs">
                        {act.pending_messages > 0 && (
                          <span className="bg-red-500/20 text-red-400 px-1.5 py-0.5 rounded-full text-[10px] font-semibold">{act.pending_messages}</span>
                        )}
                        <span className="text-gray-600">{act.activity?.status || 'unknown'}</span>
                      </div>
                    </div>
                    <div className="p-3 font-mono text-xs whitespace-pre-wrap max-h-[280px] overflow-y-auto bg-[#0d1117] text-green-400/90 leading-relaxed"
                         ref={el => { if (el) paneRefs.current.set(act.name, el); else paneRefs.current.delete(act.name); }}
                         style={{ fontFamily: "'JetBrains Mono', monospace" }}>
                      {act.activity?.pane_content || (
                        <span className="text-gray-700">No activity data</span>
                      )}
                    </div>
                    {act.activity?.timestamp && (
                      <div className="px-4 py-2 text-xs text-gray-600 border-t border-white/5">
                        {new Date(act.activity.timestamp).toLocaleTimeString()}
                      </div>
                    )}
                  </div>
                )
              })}
            </div>
          </div>
        ))}
        {activities.length === 0 && (
          <div className="card py-12 text-center text-gray-600">
            No VMs with activity data.
          </div>
        )}
      </div>

      {/* Alert feed sidebar */}
      <div className="w-80 flex-shrink-0 hidden xl:block">
        <div className="sticky top-20">
          <h2 className="text-sm font-semibold text-gray-400 uppercase tracking-wider mb-3">Alerts</h2>
          <div className="bg-gray-900/80 border border-gray-800/80 rounded-xl overflow-hidden max-h-[calc(100vh-8rem)] overflow-y-auto">
            {alerts.length === 0 ? (
              <div className="p-4 text-sm text-gray-600">No alerts</div>
            ) : (
              <div className="divide-y divide-white/5">
                {[...alerts].reverse().map((evt, i) => (
                  <div key={`${evt.timestamp}-${i}`} className="px-3 py-2 text-xs">
                    <div className="flex items-center gap-1.5">
                      <span>{alertIcon(evt.type)}</span>
                      <Link href={`/vms/${evt.name}`} className="text-blue-400 hover:text-blue-300 font-medium">{evt.name}</Link>
                    </div>
                    <div className="text-gray-500 mt-0.5">{evt.message}</div>
                    <div className="text-gray-700 mt-0.5">{new Date(evt.timestamp).toLocaleTimeString()}</div>
                  </div>
                ))}
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
