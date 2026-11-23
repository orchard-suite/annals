# Ion

An Ion extension registers a handler on the `tool_call` hook, which fires
when the model asks for a tool, with `ToolCallInfo{ToolName, ToolID, Input}`
(`docs/hooks/reference.md`). The hook's context carries `ctx.traceId`,
`ctx.runId`, `ctx.conversationId`, `ctx.sessionKey`, and `ctx.model`
(`docs/extensions/sdk-typescript.md`, "Tracing and correlation").
`permission_denied` fires when a rule refuses one, with
`PermissionDeniedInfo{ToolName, Input, Reason}`.

The extension posts one event per tool call and one per refusal. The
tool's input is never sent; only the kind of thing it points at. The
`event_key` is the run and tool call id, so a redelivered hook is stored
once.

```typescript
import { createIon } from '@ion/sdk'

const ion = createIon()
const url = process.env.ANNALS_URL ?? 'https://annals.example.com'
const token = process.env.ANNALS_TOKEN ?? ''

// What a tool touches, as a shape: a path becomes a directory or file id,
// a command becomes its first word. Values stay out.
function target(tool: string, input: Record<string, unknown>) {
  switch (tool) {
    case 'bash': return { system: 'workstation', kind: 'command', id: String(input.command ?? '').split(/\s+/)[0] || 'sh' }
    case 'read': case 'write': case 'edit': return { system: 'workstation', kind: 'file', id: String(input.file_path ?? input.path ?? '') }
    case 'grep': case 'glob': return { system: 'workstation', kind: 'directory', id: String(input.path ?? process.cwd()) }
    default: return { system: 'ion', kind: 'tool', id: tool }
  }
}

async function post(ctx: any, event: Record<string, unknown>) {
  if (!token) return
  const headers: Record<string, string> = { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` }
  if (ctx.traceId) headers.traceparent = `00-${ctx.traceId}-${ctx.traceId.slice(0, 16)}-01`
  const res = await fetch(`${url}/v1/events`, { method: 'POST', headers, body: JSON.stringify(event) })
  if (res.status === 422) ctx.log?.warn('annals refused a tool event', await res.json())
}

function base(ctx: any) {
  return {
    at: new Date().toISOString(),
    source: 'ion',
    actor: { kind: 'agent', id: `ion:conversation:${ctx.conversationId || ctx.sessionKey}`, name: 'Ion session' },
    agent: { runtime: 'ion', model: ctx.model?.id, session_id: ctx.sessionKey, harness: 'ion-engine' },
    trace_id: ctx.traceId || undefined,
  }
}

ion.on('tool_call', async (ctx, info) => {
  await post(ctx, {
    ...base(ctx),
    event_key: `run:${ctx.runId}:tool:${info.toolID}`,
    action: { verb: `tool.${info.toolName.toLowerCase()}`, target: target(info.toolName.toLowerCase(), info.input ?? {}), outcome: 'ok' },
    labels: { hook: 'tool_call', tool: info.toolName.toLowerCase() },
  })
  return null // observe only; never block from here
})

ion.on('permission_denied', async (ctx, info) => {
  await post(ctx, {
    ...base(ctx),
    event_key: `run:${ctx.runId}:denied:${info.toolName}:${Date.now()}`,
    action: {
      verb: `tool.${info.toolName.toLowerCase()}`,
      target: target(info.toolName.toLowerCase(), info.input ?? {}),
      outcome: 'refused',
      reason: String(info.reason ?? 'permission denied').slice(0, 512),
    },
    labels: { hook: 'permission_denied', tool: info.toolName.toLowerCase() },
  })
  return null
})
```

`tool_call` is a blocking hook in Ion; the handler returns `null` so
Annals never decides whether a tool runs. Posting is awaited so the event
leaves before the tool does; an Annals that is down costs one timeout per
call, so set `ANNALS_TOKEN` only where that is acceptable, or post from a
queue.
