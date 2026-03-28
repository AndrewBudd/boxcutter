'use client'

import { useEffect, useState } from 'react'
import Link from 'next/link'
import type { TapegunActivity } from '@/lib/types'

export default function ActivityPage() {
  const [activities, setActivities] = useState<TapegunActivity[]>([])

  useEffect(() => {
    const poll = async () => {
      try {
        const data = await fetch('/api/tapegun/activity').then(r => r.json())
        setActivities(Array.isArray(data) ? data : [])
      } catch {}
    }
    poll()
    const interval = setInterval(poll, 5000)
    return () => clearInterval(interval)
  }, [])

  return (
    <div>
      <div className="mb-6">
        <h1 className="text-2xl font-semibold">Activity</h1>
        <p className="text-sm text-gray-500 mt-0.5">Live view of VM agent activity</p>
      </div>

      <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
        {activities.map(act => (
          <div key={act.name} className="bg-gray-900/80 border border-gray-800/80 rounded-xl overflow-hidden">
            <div className="flex items-center justify-between px-4 py-2.5 border-b border-white/5 bg-white/[0.02]">
              <div className="flex items-center gap-2">
                <Link href={`/vms/${act.name}`} className="font-medium text-blue-400 hover:text-blue-300 transition-colors">
                  {act.name}
                </Link>
                <span className="text-xs text-gray-600">{act.node_name?.replace('boxcutter-', '')}</span>
              </div>
              <div className="flex items-center gap-2 text-xs">
                {act.pending_messages > 0 && (
                  <span className="bg-red-500/20 text-red-400 px-1.5 py-0.5 rounded-full text-[10px] font-semibold">{act.pending_messages}</span>
                )}
                <span className={`w-1.5 h-1.5 rounded-full ${act.vm_status === 'running' ? 'bg-emerald-400' : 'bg-gray-600'}`} />
              </div>
            </div>
            <div className="p-3 font-mono text-xs whitespace-pre-wrap max-h-[280px] overflow-y-auto bg-[#0d1117] text-green-400/90 leading-relaxed"
                 style={{ fontFamily: "'JetBrains Mono', monospace" }}>
              {act.activity?.pane_content || (
                <span className="text-gray-700">No activity data</span>
              )}
            </div>
            {act.activity?.status && (
              <div className="px-4 py-2 text-xs text-gray-600 border-t border-white/5 flex items-center justify-between">
                <span>{act.activity.status}</span>
                {act.activity.timestamp && (
                  <span>{new Date(act.activity.timestamp).toLocaleTimeString()}</span>
                )}
              </div>
            )}
          </div>
        ))}
        {activities.length === 0 && (
          <div className="col-span-2 card py-12 text-center text-gray-600">
            No VMs with activity data. Install the Tapegun plugin in your VMs to see live activity.
          </div>
        )}
      </div>
    </div>
  )
}
