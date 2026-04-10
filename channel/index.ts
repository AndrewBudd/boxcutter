#!/usr/bin/env bun
/**
 * boxcutter-channel — Claude Code channel MCP server.
 *
 * Connects to the vmid metadata service SSE stream and pushes structured
 * events into Claude Code's context as <channel> tags. Exposes reply/status
 * tools so the agent can communicate back through a typed interface.
 *
 * Runs as an MCP server subprocess of Claude Code, communicating over stdio.
 */

import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";

const METADATA_BASE = process.env.METADATA_URL || "http://169.254.169.254";

// ---------------------------------------------------------------------------
// MCP Server
// ---------------------------------------------------------------------------

const server = new Server(
  { name: "boxcutter-channel", version: "0.1.0" },
  {
    capabilities: {
      tools: {},
      experimental: { "claude/channel": {} },
    },
    instructions: `Events from the boxcutter infrastructure arrive as <channel source="boxcutter" type="..." ...>.

Event types and expected behavior:
- type="upgrade_starting": Save work, commit changes. No reply needed.
- type="drain_imminent": Pause long-running operations. No reply needed.
- type="ci_failure": Investigate and fix. Use boxcutter_status to report progress.
- type="ci_passed": Note that CI passed. No action needed unless awaiting review.
- type="health_alert": Take corrective action if applicable.
- type="work_assigned": Begin working on the assigned issue.
- type="message" with reply_expected="true": Reply using boxcutter_reply with the chat_id.
- type="message" with reply_expected="false": Acknowledge by reading; no reply needed.

Use boxcutter_status to report what you're working on whenever your activity changes.
Use boxcutter_escalate when you're blocked or need human intervention.`,
  }
);

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

server.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: [
    {
      name: "boxcutter_reply",
      description: "Reply to a message or acknowledge an event",
      inputSchema: {
        type: "object" as const,
        properties: {
          chat_id: {
            type: "string",
            description: "The event ID or chat_id from the inbound <channel> tag",
          },
          text: { type: "string", description: "Reply text" },
        },
        required: ["chat_id", "text"],
      },
    },
    {
      name: "boxcutter_status",
      description: "Report agent status to the orchestrator",
      inputSchema: {
        type: "object" as const,
        properties: {
          status: {
            type: "string",
            enum: ["active", "idle", "blocked", "reviewing", "testing"],
            description: "Current agent activity status",
          },
          summary: {
            type: "string",
            description: "What the agent is working on (1-2 sentences)",
          },
          pr_number: {
            type: "number",
            description: "PR number if working on one",
          },
        },
        required: ["status"],
      },
    },
    {
      name: "boxcutter_escalate",
      description: "Escalate an issue to the eng-manager or a specific agent",
      inputSchema: {
        type: "object" as const,
        properties: {
          to: {
            type: "string",
            description: 'Target: "eng-manager" or a VM name',
          },
          subject: { type: "string" },
          body: { type: "string" },
          urgency: {
            type: "string",
            enum: ["normal", "high", "critical"],
          },
        },
        required: ["to", "subject", "body"],
      },
    },
  ],
}));

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  const { name, arguments: args } = request.params;

  switch (name) {
    case "boxcutter_reply": {
      const { chat_id, text } = args as { chat_id: string; text: string };
      await postReply({ chat_id, text, timestamp: new Date().toISOString() });
      return { content: [{ type: "text", text: `Reply sent (chat_id: ${chat_id})` }] };
    }

    case "boxcutter_status": {
      const { status, summary, pr_number } = args as {
        status: string;
        summary?: string;
        pr_number?: number;
      };
      await postReply({
        status,
        summary: summary || "",
        text: `Status: ${status}${summary ? " — " + summary : ""}`,
        timestamp: new Date().toISOString(),
      });
      return {
        content: [{ type: "text", text: `Status reported: ${status}` }],
      };
    }

    case "boxcutter_escalate": {
      const { to, subject, body, urgency } = args as {
        to: string;
        subject: string;
        body: string;
        urgency?: string;
      };
      await postReply({
        chat_id: `escalate-${Date.now()}`,
        text: JSON.stringify({ to, subject, body, urgency: urgency || "normal" }),
        status: "blocked",
        timestamp: new Date().toISOString(),
      });
      return {
        content: [{ type: "text", text: `Escalated to ${to}: ${subject}` }],
      };
    }

    default:
      return { content: [{ type: "text", text: `Unknown tool: ${name}` }], isError: true };
  }
});

// ---------------------------------------------------------------------------
// SSE event stream from vmid metadata service
// ---------------------------------------------------------------------------

interface ChannelEvent {
  id: string;
  type: string;
  source: string;
  urgency?: string;
  body: string;
  attrs?: Record<string, string>;
  timestamp: string;
}

async function connectSSE(): Promise<void> {
  const url = `${METADATA_BASE}/channel/events`;

  while (true) {
    try {
      const response = await fetch(url, {
        headers: { Accept: "text/event-stream" },
        signal: AbortSignal.timeout(0), // no timeout — long-lived
      });

      if (!response.ok || !response.body) {
        console.error(`SSE connect failed: ${response.status}`);
        await sleep(5000);
        continue;
      }

      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;

        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split("\n");
        buffer = lines.pop() || "";

        for (const line of lines) {
          if (line.startsWith("data: ")) {
            const data = line.slice(6);
            try {
              const evt: ChannelEvent = JSON.parse(data);
              await pushEvent(evt);
            } catch {
              // skip malformed events
            }
          }
        }
      }
    } catch (err) {
      console.error(`SSE error: ${err}`);
    }

    // Reconnect after disconnect
    await sleep(3000);
  }
}

async function pushEvent(evt: ChannelEvent): Promise<void> {
  // Build attribute string for the <channel> tag
  const attrs: string[] = [
    `source="${evt.source || "boxcutter"}"`,
    `type="${evt.type}"`,
  ];
  if (evt.id) attrs.push(`id="${evt.id}"`);
  if (evt.urgency) attrs.push(`urgency="${evt.urgency}"`);
  if (evt.attrs) {
    for (const [k, v] of Object.entries(evt.attrs)) {
      attrs.push(`${k}="${v}"`);
    }
  }

  try {
    await server.notification({
      method: "notifications/claude/channel",
      params: {
        channel: "boxcutter",
        content: evt.body,
        meta: {
          type: evt.type,
          source: evt.source,
          urgency: evt.urgency || "normal",
          id: evt.id,
          ...evt.attrs,
        },
      },
    });
  } catch (err) {
    console.error(`Failed to push event: ${err}`);
  }
}

// ---------------------------------------------------------------------------
// Outbound replies to metadata service
// ---------------------------------------------------------------------------

async function postReply(reply: Record<string, unknown>): Promise<void> {
  try {
    await fetch(`${METADATA_BASE}/channel/reply`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(reply),
    });
  } catch (err) {
    console.error(`Failed to post reply: ${err}`);
  }
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

async function main() {
  const transport = new StdioServerTransport();
  await server.connect(transport);

  // Start SSE listener in background (doesn't block server startup)
  connectSSE().catch((err) => console.error(`SSE fatal: ${err}`));
}

main().catch((err) => {
  console.error(`Fatal: ${err}`);
  process.exit(1);
});
