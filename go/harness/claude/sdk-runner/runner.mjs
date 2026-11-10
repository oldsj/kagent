// Agent SDK bridge for the Claude Harness (KAGENT_CLAUDE_DRIVER=sdk).
//
// kagent-claude starts this process lazily and owns everything around it: A2A,
// TaskStore, Git preparation, the continuation store, client WebSockets, the
// event backlog and activity reports. This file only holds one long-lived SDK
// query() in streaming-input mode and relays it over stdio as JSONL:
//
//   stdin  (Go -> runner): first line {op:"start", options}, then
//          {op:"user", uuid, text} | {op:"interrupt"|"model"|"effort"|"mode", ref, ...}
//          | {op:"perm", id, behavior, always?, message?}
//   stdout (runner -> Go): {k:"init"} | {k:"sdk", m} | {k:"perm", ...} | {k:"perm_aborted", id}
//          | {k:"ack", ref, what, ms, error?} | {k:"ended", error?}
//
// The protocol is private to the harness image; the public client protocol is
// served by Go. Nothing here listens on a socket or reads credentials.
import { randomUUID } from 'node:crypto';
import readline from 'node:readline';
import { pathToFileURL } from 'node:url';

// Async queue the SDK reads user messages from for the life of the process.
export function messageQueue() {
  const pending = [];
  let wake = null;
  let closed = false;
  return {
    push(message) { pending.push(message); if (wake) wake(); },
    close() { closed = true; if (wake) wake(); },
    async *[Symbol.asyncIterator]() {
      for (;;) {
        while (pending.length) yield pending.shift();
        if (closed) return;
        await new Promise((resolve) => { wake = resolve; });
        wake = null;
      }
    },
  };
}

// sdkOptions maps the harness launch spec onto SDK options. Every field is
// compiled by Go from the same adapter output the process driver uses.
export function sdkOptions(spec, canUseTool) {
  const options = {
    cwd: spec.cwd,
    env: spec.env,
    pathToClaudeCodeExecutable: spec.executable,
    includePartialMessages: true,
    canUseTool,
  };
  if (spec.settingSources) options.settingSources = spec.settingSources;
  if (spec.settings) options.settings = spec.settings;
  if (spec.model) options.model = spec.model;
  if (spec.effort) options.effort = spec.effort;
  if (spec.resume) options.resume = spec.resume;
  if (spec.permissionMode) {
    options.permissionMode = spec.permissionMode;
    if (spec.permissionMode === 'bypassPermissions') options.allowDangerouslySkipPermissions = true;
  }
  if (spec.strictMcpConfig) options.strictMcpConfig = true;
  if (spec.mcpServers) options.mcpServers = spec.mcpServers;
  if (spec.disallowedTools?.length) options.disallowedTools = spec.disallowedTools;
  if (spec.additionalDirectories?.length) options.additionalDirectories = spec.additionalDirectories;
  if (spec.pluginDirs?.length) options.plugins = spec.pluginDirs.map((path) => ({ type: 'local', path }));
  if (spec.agents) options.agents = spec.agents;
  options.systemPrompt = spec.appendSystemPrompt
    ? { type: 'preset', preset: 'claude_code', append: spec.appendSystemPrompt }
    : { type: 'preset', preset: 'claude_code' };
  return options;
}

// createRunner wires one query to the Go parent. send writes one event line.
export function createRunner({ query, send }) {
  const input = messageQueue();
  const perms = new Map(); // id -> { input, suggestions, resolve }
  let q = null;

  function canUseTool(toolName, toolInput, opts) {
    const id = randomUUID();
    return new Promise((resolve) => {
      perms.set(id, { input: toolInput, suggestions: opts.suggestions, resolve });
      send({
        k: 'perm', id, toolName, input: toolInput, title: opts.title, description: opts.description,
        decisionReason: opts.decisionReason, blockedPath: opts.blockedPath, suggestions: opts.suggestions,
        toolUseID: opts.toolUseID,
      });
      opts.signal?.addEventListener('abort', () => {
        if (perms.delete(id)) {
          send({ k: 'perm_aborted', id });
          resolve({ behavior: 'deny', message: 'The request was aborted.' });
        }
      });
    });
  }

  async function start(spec) {
    q = query({ prompt: input, options: sdkOptions(spec, canUseTool) });
    q.initializationResult().then(async (init) => {
      let models = init.models;
      if (!models?.length) models = await q.supportedModels().catch(() => undefined);
      send({ k: 'init', commands: init.commands, models, account: init.account ? { subscriptionType: init.account.subscriptionType } : undefined });
    }).catch((e) => process.stderr.write(`sdk-runner: initialization result: ${e.message}\n`));
    try {
      for await (const m of q) send({ k: 'sdk', m });
      send({ k: 'ended' });
    } catch (e) {
      send({ k: 'ended', error: String(e?.message || e) });
    }
  }

  async function control(op) {
    const t = Date.now();
    const ack = (extra = {}) => send({ k: 'ack', ref: op.ref, what: op.op, ms: Date.now() - t, ...extra });
    try {
      switch (op.op) {
        case 'user':
          input.push({ type: 'user', uuid: op.uuid, session_id: '', parent_tool_use_id: null, message: { role: 'user', content: op.text } });
          return;
        case 'interrupt': await q.interrupt(); return ack();
        case 'model': await q.setModel(op.model || undefined); return ack({ model: op.model });
        // An empty effort clears the override: null returns to the model's default.
        case 'effort': await q.applyFlagSettings({ effortLevel: op.effort || null }); return ack({ effort: op.effort });
        case 'mode': await q.setPermissionMode(op.mode); return ack({ mode: op.mode });
        case 'perm': {
          const p = perms.get(op.id);
          if (!p) return;
          perms.delete(op.id);
          p.resolve(op.behavior === 'allow'
            ? { behavior: 'allow', updatedInput: p.input, ...(op.always && p.suggestions ? { updatedPermissions: p.suggestions } : {}) }
            : { behavior: 'deny', message: op.message || 'The user declined this tool call.' });
          return;
        }
        default:
          return ack({ error: `unknown op ${op.op}` });
      }
    } catch (e) {
      return ack({ error: String(e?.message || e) });
    }
  }

  // handle consumes one parsed stdin line. The first must be start.
  async function handle(op) {
    if (!q) {
      if (op.op !== 'start') throw new Error('first operation must be start');
      start(op.options);
      return;
    }
    await control(op);
  }

  return { handle, close: () => input.close() };
}

async function main() {
  const { query } = await import('@anthropic-ai/claude-agent-sdk');
  const send = (o) => process.stdout.write(JSON.stringify(o) + '\n');
  const runner = createRunner({ query, send });
  const lines = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
  for await (const line of lines) {
    if (!line.trim()) continue;
    let op;
    try { op = JSON.parse(line); } catch { process.stderr.write('sdk-runner: malformed operation\n'); continue; }
    // Operations apply in order (a model change lands before the next prompt).
    // start returns at once; controls resolve quickly.
    try { await runner.handle(op); } catch (e) { process.stderr.write(`sdk-runner: ${e.message}\n`); process.exit(2); }
  }
  // The parent closed stdin: it is stopping this runner.
  runner.close();
  process.exit(0);
}

if (import.meta.url === pathToFileURL(process.argv[1] ?? '').href) main();
