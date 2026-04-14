'use client'

import { useEffect, useState } from 'react'

interface AgentMemory {
  name: string
  node: string
  status: string
  activity?: { status: string; pane_content: string; timestamp: string }
  files?: { branch: string; files: string[] }
  checkpoint?: { session_id: string; size_bytes: number; timestamp: string }
  config?: { tool: string; model: string; repos: string[]; persona_role: string }
}

type StatusColor = 'green' | 'yellow' | 'gray'

function agentStatusColor(status: string): StatusColor {
  if (status === 'active') return 'green'
  if (status === 'idle') return 'yellow'
  return 'gray'
}

function StatusDot({ color }: { color: StatusColor }) {
  const cls: Record<StatusColor, string> = {
    green: 'bg-emerald-400 shadow-sm shadow-emerald-400/50',
    yellow: 'bg-yellow-400 shadow-sm shadow-yellow-400/50',
    gray: 'bg-gray-600',
  }
  return <span className={`w-2 h-2 rounded-full inline-block flex-shrink-0 ${cls[color]}`} />
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

function formatTime(iso: string): string {
  try {
    return new Date(iso).toLocaleTimeString()
  } catch {
    return iso
  }
}

function SectionLabel({ children }: { children: React.ReactNode }) {
  return (
    <div className="text-[10px] font-semibold uppercase tracking-widest text-gray-600 mb-1.5">
      {children}
    </div>
  )
}

function FileOverlapWarning({ agents }: { agents: AgentMemory[] }) {
  // Build map of file -> agents touching it
  const fileAgents: Record<string, string[]> = {}
  for (const agent of agents) {
    for (const f of agent.files?.files ?? []) {
      if (!fileAgents[f]) fileAgents[f] = []
      fileAgents[f].push(agent.name)
    }
  }
  const overlaps = Object.entries(fileAgents).filter(([, names]) => names.length > 1)

  if (overlaps.length === 0) return null

  return (
    <div className="mb-6 bg-yellow-500/10 border border-yellow-500/20 rounded-xl p-4">
      <div className="flex items-center gap-2 mb-3">
        <span className="text-yellow-400 text-sm font-semibold">File Conflicts Detected</span>
        <span className="text-[10px] font-semibold uppercase tracking-widest text-yellow-600 bg-yellow-500/10 border border-yellow-500/20 px-1.5 py-0.5 rounded">
          {overlaps.length} {overlaps.length === 1 ? 'file' : 'files'}
        </span>
      </div>
      <div className="flex flex-col gap-2">
        {overlaps.map(([file, names]) => (
          <div key={file} className="flex items-start gap-3">
            <span className="font-mono text-xs text-yellow-300/80 truncate flex-1">{file}</span>
            <div className="flex items-center gap-1 flex-shrink-0">
              {names.map(n => (
                <span key={n} className="text-[10px] px-1.5 py-0.5 rounded bg-yellow-500/15 text-yellow-400 border border-yellow-500/20 font-mono">
                  {n}
                </span>
              ))}
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

function AgentCard({ agent }: { agent: AgentMemory }) {
  const color = agentStatusColor(agent.status)
  const panePreview = agent.activity?.pane_content
    ? agent.activity.pane_content.slice(-800)
    : null

  return (
    <div className="bg-gray-900/80 border border-gray-800/80 rounded-xl overflow-hidden flex flex-col">
      {/* Card header */}
      <div className="flex items-center justify-between px-4 py-3 border-b border-white/5 bg-white/[0.02]">
        <div className="flex items-center gap-2 min-w-0">
          <StatusDot color={color} />
          <span className="font-medium text-white truncate" style={{ fontFamily: "'JetBrains Mono', monospace" }}>
            {agent.name}
          </span>
        </div>
        <div className="flex items-center gap-2 flex-shrink-0 ml-2">
          {agent.node && (
            <span className="text-[10px] px-1.5 py-0.5 rounded bg-blue-500/10 text-blue-400 border border-blue-500/15 font-mono">
              {agent.node.replace('boxcutter-', '')}
            </span>
          )}
          <span className={`text-[10px] px-1.5 py-0.5 rounded font-medium ${
            color === 'green'
              ? 'bg-emerald-500/10 text-emerald-400 border border-emerald-500/20'
              : color === 'yellow'
              ? 'bg-yellow-500/10 text-yellow-400 border border-yellow-500/20'
              : 'bg-gray-700/30 text-gray-500 border border-gray-700/40'
          }`}>
            {agent.status || 'unknown'}
          </span>
        </div>
      </div>

      <div className="p-4 flex flex-col gap-4 flex-1">
        {/* Current Context */}
        <div>
          <SectionLabel>Current Context</SectionLabel>
          {agent.files ? (
            <div>
              <div className="flex items-center gap-1.5 mb-1.5">
                <span className="text-[10px] text-gray-600">branch</span>
                <span className="font-mono text-xs text-purple-400">{agent.files.branch}</span>
              </div>
              {agent.files.files.length > 0 ? (
                <div className="flex flex-col gap-0.5">
                  {agent.files.files.map(f => (
                    <div key={f} className="font-mono text-xs text-gray-400 flex items-center gap-1.5">
                      <span className="text-gray-700">—</span>
                      <span className="truncate">{f}</span>
                    </div>
                  ))}
                </div>
              ) : (
                <div className="text-xs text-gray-600">No files tracked</div>
              )}
            </div>
          ) : (
            <div className="text-xs text-gray-600">No context data</div>
          )}
        </div>

        {/* Session History */}
        <div>
          <SectionLabel>Session History</SectionLabel>
          {agent.checkpoint ? (
            <div className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs">
              <div className="text-gray-500">Session ID</div>
              <div className="font-mono text-gray-400 truncate" title={agent.checkpoint.session_id}>
                {agent.checkpoint.session_id.slice(0, 12)}…
              </div>
              <div className="text-gray-500">Context size</div>
              <div className="font-mono text-gray-300">{formatBytes(agent.checkpoint.size_bytes)}</div>
              <div className="text-gray-500">Last checkpoint</div>
              <div className="font-mono text-gray-400">{formatTime(agent.checkpoint.timestamp)}</div>
            </div>
          ) : (
            <div className="text-xs text-gray-600">No session data</div>
          )}
        </div>

        {/* Knowledge */}
        <div>
          <SectionLabel>Knowledge</SectionLabel>
          {agent.config ? (
            <div className="grid grid-cols-2 gap-x-4 gap-y-1 text-xs">
              <div className="text-gray-500">Tool</div>
              <div className="font-mono text-gray-300">{agent.config.tool}</div>
              <div className="text-gray-500">Model</div>
              <div className="font-mono text-gray-300 truncate" title={agent.config.model}>{agent.config.model}</div>
              <div className="text-gray-500">Role</div>
              <div className="font-mono text-gray-300">{agent.config.persona_role}</div>
              {agent.config.repos.length > 0 && (
                <>
                  <div className="text-gray-500 self-start">Repos</div>
                  <div className="flex flex-col gap-0.5">
                    {agent.config.repos.map(r => (
                      <div key={r} className="font-mono text-blue-400/80 truncate" title={r}>{r}</div>
                    ))}
                  </div>
                </>
              )}
            </div>
          ) : (
            <div className="text-xs text-gray-600">No config data</div>
          )}
        </div>

        {/* Recent Activity */}
        <div className="flex-1 flex flex-col">
          <SectionLabel>Recent Activity</SectionLabel>
          {agent.activity ? (
            <div className="flex flex-col gap-2 flex-1">
              <div className="flex items-center gap-2">
                <StatusDot color={agentStatusColor(agent.activity.status)} />
                <span className="text-xs text-gray-400">{agent.activity.status}</span>
                <span className="text-xs text-gray-700 ml-auto">{formatTime(agent.activity.timestamp)}</span>
              </div>
              {panePreview && (
                <div
                  className="bg-[#0d1117] rounded-lg p-2.5 font-mono text-[11px] text-green-400/90 whitespace-pre-wrap overflow-hidden leading-relaxed max-h-32 overflow-y-auto"
                  style={{ fontFamily: "'JetBrains Mono', monospace" }}
                >
                  {panePreview}
                </div>
              )}
            </div>
          ) : (
            <div className="text-xs text-gray-600">No activity data</div>
          )}
        </div>
      </div>
    </div>
  )
}

export default function MemoryPage() {
  const [agents, setAgents] = useState<AgentMemory[]>([])
  const [loaded, setLoaded] = useState(false)

  useEffect(() => {
    const poll = async () => {
      try {
        const data = await fetch('/api/memory').then(r => r.json())
        if (Array.isArray(data)) {
          setAgents([...data].sort((a, b) => a.name.localeCompare(b.name)))
        }
        setLoaded(true)
      } catch {
        setLoaded(true)
      }
    }
    poll()
    const id = setInterval(poll, 10000)
    return () => clearInterval(id)
  }, [])

  const active = agents.filter(a => a.status === 'active').length
  const idle = agents.filter(a => a.status === 'idle').length

  return (
    <div>
      {/* Header */}
      <div className="flex items-center justify-between mb-6">
        <div>
          <h1 className="text-2xl font-semibold">Memory &amp; Context</h1>
          <p className="text-sm text-gray-500 mt-0.5">What your agents know</p>
        </div>
        {loaded && agents.length > 0 && (
          <div className="flex items-center gap-3 text-xs">
            <span className="flex items-center gap-1.5 text-gray-500">
              <span className="w-2 h-2 rounded-full bg-gray-700 inline-block" />
              {agents.length} agents
            </span>
            {active > 0 && (
              <span className="flex items-center gap-1.5 text-emerald-400">
                <span className="w-2 h-2 rounded-full bg-emerald-400 inline-block" />
                {active} active
              </span>
            )}
            {idle > 0 && (
              <span className="flex items-center gap-1.5 text-yellow-400">
                <span className="w-2 h-2 rounded-full bg-yellow-400 inline-block" />
                {idle} idle
              </span>
            )}
          </div>
        )}
      </div>

      {/* File overlap warning */}
      {loaded && agents.length > 0 && <FileOverlapWarning agents={agents} />}

      {/* Agent cards */}
      {!loaded && (
        <div className="text-center py-16 text-gray-600">Loading...</div>
      )}
      {loaded && agents.length === 0 && (
        <div className="bg-gray-900/80 border border-gray-800/80 rounded-xl py-16 text-center text-gray-600">
          No agent memory data available.
        </div>
      )}
      {agents.length > 0 && (
        <div className="grid grid-cols-1 lg:grid-cols-2 xl:grid-cols-3 gap-4">
          {agents.map(agent => (
            <AgentCard key={agent.name} agent={agent} />
          ))}
        </div>
      )}
    </div>
  )
}
