import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createRunner, sdkOptions } from './runner.mjs';

// fakeQuery records options and echoes each user message as a result, the way
// the CLI binds results to the uuids it consumed.
function fakeQuery(calls) {
  return ({ prompt, options }) => {
    calls.options = options;
    calls.controls = [];
    const it = (async function* () {
      yield { type: 'system', subtype: 'init', session_id: 'sess-1' };
      for await (const msg of prompt) {
        if (msg.message.content === 'ask') {
          const decision = await options.canUseTool('Bash', { command: 'touch x' }, { suggestions: [{ type: 'addRules' }], signal: new AbortController().signal });
          yield { type: 'assistant', decision };
        }
        yield { type: 'result', subtype: 'success', user_message_uuids: [msg.uuid] };
      }
    })();
    return Object.assign(it, {
      initializationResult: async () => ({ commands: [{ name: 'help' }], models: [{ value: 'opus' }], account: { subscriptionType: 'max', email: 'x' } }),
      supportedModels: async () => [],
      interrupt: async () => calls.controls.push('interrupt'),
      setModel: async (m) => calls.controls.push(`model:${m}`),
      applyFlagSettings: async (s) => calls.controls.push(`effort:${s.effortLevel}`),
      setPermissionMode: async (m) => { if (m === 'bad') throw new Error('nope'); calls.controls.push(`mode:${m}`); },
    });
  };
}

const tick = () => new Promise((r) => setTimeout(r, 5));

test('relays turns, controls and permission answers', async () => {
  const calls = {};
  const out = [];
  const runner = createRunner({ query: fakeQuery(calls), send: (o) => out.push(o) });
  await runner.handle({ op: 'start', options: { cwd: '/w', executable: '/usr/local/bin/claude', resume: 'sess-1', permissionMode: 'bypassPermissions' } });
  await runner.handle({ op: 'user', uuid: 'u1', text: 'hi' });
  await tick();
  assert.equal(calls.options.resume, 'sess-1');
  assert.equal(calls.options.allowDangerouslySkipPermissions, true);
  const init = out.find((o) => o.k === 'init');
  assert.deepEqual(init.account, { subscriptionType: 'max' }, 'account details other than the plan stay in the runner');
  assert.deepEqual(out.filter((o) => o.k === 'sdk').map((o) => o.m.type), ['system', 'result']);
  assert.deepEqual(out.find((o) => o.m?.type === 'result').m.user_message_uuids, ['u1']);

  await runner.handle({ op: 'user', uuid: 'u2', text: 'ask' });
  await tick();
  const perm = out.find((o) => o.k === 'perm');
  assert.equal(perm.toolName, 'Bash');
  await runner.handle({ op: 'perm', id: perm.id, behavior: 'allow', always: true });
  await tick();
  const decided = out.find((o) => o.m?.type === 'assistant').m.decision;
  assert.deepEqual(decided, { behavior: 'allow', updatedInput: { command: 'touch x' }, updatedPermissions: [{ type: 'addRules' }] });

  await runner.handle({ op: 'model', ref: 'r1', model: 'sonnet' });
  await runner.handle({ op: 'effort', ref: 'r2', effort: 'high' });
  await runner.handle({ op: 'mode', ref: 'r3', mode: 'bad' });
  await runner.handle({ op: 'interrupt', ref: 'r4' });
  assert.deepEqual(calls.controls, ['model:sonnet', 'effort:high', 'interrupt']);
  const acks = out.filter((o) => o.k === 'ack');
  assert.deepEqual(acks.map((a) => [a.ref, a.error ?? null]), [['r1', null], ['r2', null], ['r3', 'nope'], ['r4', null]]);
  runner.close();
  await tick();
  assert.equal(out.at(-1).k, 'ended');
});

test('first operation must be start', async () => {
  const runner = createRunner({ query: fakeQuery({}), send: () => {} });
  await assert.rejects(runner.handle({ op: 'user', uuid: 'u', text: 'x' }), /start/);
});

test('maps the launch spec onto SDK options', () => {
  const options = sdkOptions({
    cwd: '/data/workspace', executable: 'claude', settingSources: ['user'], settings: '{"disableAllHooks":true}',
    strictMcpConfig: true, mcpServers: { tools: { type: 'http', url: 'http://x' } }, disallowedTools: ['Monitor'], additionalDirectories: ['/data/workspace'],
    pluginDirs: ['/p1', '/p2'], agents: { a: { description: 'd', prompt: 'p' } }, appendSystemPrompt: 'extra', model: 'opus',
  }, () => {});
  assert.deepEqual(options.mcpServers, { tools: { type: 'http', url: 'http://x' } });
  assert.deepEqual(options.plugins, [{ type: 'local', path: '/p1' }, { type: 'local', path: '/p2' }]);
  assert.deepEqual(options.systemPrompt, { type: 'preset', preset: 'claude_code', append: 'extra' });
  assert.equal(options.strictMcpConfig, true);
  assert.equal(options.pathToClaudeCodeExecutable, 'claude');
  assert.equal(options.allowDangerouslySkipPermissions, undefined);
});
