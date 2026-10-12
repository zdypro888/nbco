'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, 'app.js'), 'utf8');

function functionSource(name) {
  const start = source.indexOf(`function ${name}(`);
  assert.ok(start >= 0, `missing ${name}`);
  const end = source.indexOf('\nfunction ', start + 1);
  return source.slice(start, end < 0 ? source.length : end);
}

function fixture(route) {
  const host = { textContent: '' };
  const menu = {};
  const calls = [];
  const state = { route, me: { id: 42 }, workspacePage: 'home' };
  const context = vm.createContext({
    URL, state, ihtmlMountGeneration: 3, ihtmlWorkspace: null,
    ihtmlMountOwner: { chatOnly: false, userID: '' }, assistantDisplay: { userID: '', messages: [] },
    root: { dataset: { cspNonce: 'host-nonce' } },
    document: { querySelector: selector => selector === '#ihtmlWorkspaceMenu' ? menu : host },
    window: { location: { origin: 'https://nbco.test' }, ihtml: {
      mount: (el, opts) => { calls.push({ mode: 'workspace', el, opts }); return { destroy() { calls.push({ mode: 'destroy' }); } }; },
      mountChat: (el, opts) => { calls.push({ mode: 'chat', el, opts }); return {
        chat: { getMessages: () => [{ role: 'user', content: 'saved question' }, { role: 'assistant', content: 'saved answer' }] },
        destroy() { calls.push({ mode: 'destroy' }); },
      }; },
    } },
    getIHTMLTicket() {}, resetIHTMLAuth() {}, normalizeWorkspacePage: value => value,
    syncBrowserLocation() {}, addLog() {},
  });
  vm.runInContext(['renderChatRoute', 'disposeIHTMLWorkspace', 'mountIHTMLWorkspace'].map(functionSource).join('\n'), context);
  return { context, state, host, menu, calls };
}

test('assistant mounts shared chat-only UI with identity-scoped ticket auth', () => {
  const { context, calls, host } = fixture('chat');
  vm.runInContext('mountIHTMLWorkspace(3)', context);
  assert.equal(calls.length, 1);
  const call = calls[0];
  assert.equal(call.mode, 'chat');
  assert.equal(call.el, host);
  assert.equal(call.opts.base, 'https://nbco.test/ui/');
  assert.equal(call.opts.storageKey, 'nbco-assistant:42');
  assert.equal(call.opts.auth, context.getIHTMLTicket);
  assert.equal(call.opts.connectionAuth, context.getIHTMLTicket);
  assert.equal(call.opts.scriptNonce, 'host-nonce');
  assert.equal(call.opts.contextProvider().page.path, '/chat');
  assert.equal(call.opts.contextProvider().page.visible_text, '');
  const html = vm.runInContext('renderChatRoute()', context);
  assert.match(html, /id="ihtmlAssistant"/);
  assert.doesNotMatch(html, /chatInput|chatLog|data-form/);
  vm.runInContext('disposeIHTMLWorkspace(); mountIHTMLWorkspace(3)', context);
  assert.equal(calls.length, 2);
  assert.equal(calls[1].mode, 'destroy');
  vm.runInContext('mountIHTMLWorkspace(4)', context);
  assert.equal(calls[2].opts.initialMessages[1].content, 'saved answer');
});

test('assistant display snapshots never migrate to another user', () => {
  const { context, state, calls } = fixture('chat');
  vm.runInContext('mountIHTMLWorkspace(3); disposeIHTMLWorkspace()', context);
  state.me.id = 99;
  vm.runInContext('mountIHTMLWorkspace(4)', context);
  assert.equal(calls[2].opts.initialMessages, undefined);
  assert.equal(calls[2].opts.storageKey, 'nbco-assistant:99');
});

test('workspace retains full mount and stale/other routes never mount', () => {
  const { context, calls, state, menu } = fixture('workspace');
  vm.runInContext('mountIHTMLWorkspace(2)', context);
  assert.equal(calls.length, 0);
  vm.runInContext('mountIHTMLWorkspace(3)', context);
  assert.equal(calls[0].mode, 'workspace');
  assert.equal(calls[0].opts.menu, menu);
  assert.equal(calls[0].opts.storageKey, 'nbco-dynamic-workspace:42');
  state.route = 'files';
  vm.runInContext('mountIHTMLWorkspace(3)', context);
  assert.equal(calls.length, 1);
});
