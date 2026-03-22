export interface VM {
  name: string
  type: string
  description?: string
  node_id: string
  node_name: string
  tailscale_ip: string
  mode: string
  vcpu: number
  ram_mib: number
  disk: string
  status: string
}

export interface TapegunActivity {
  name: string
  node_id: string
  node_name: string
  vm_status: string
  activity?: {
    timestamp: string
    pane_content: string
    status: string
    summary?: string
  }
  last_status?: {
    timestamp: string
    status: string
  }
  pending_messages: number
}

export interface Node {
  id: string
  tailscale_name: string
  tailscale_ip: string
  bridge_ip: string
  api_addr: string
  status: string
  registered_at: string
  last_heartbeat: string
}
