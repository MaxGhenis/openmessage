const { test, expect } = require('@playwright/test');
const { controlToken } = require('./control-token');

// A WhatsApp client picks its own stanza ids, and a reply carries the id it
// quotes, so both ids below are what a stranger can put in front of the UI.
// Before the fix, the reply preview and the reaction buttons put these ids
// inside inline onclick="f('...')" handlers, where the HTML parser's entity
// decoding let the quote close the string and the rest run as code.
const HOSTILE_ID = `x');globalThis.__omXss=1;//"><img src=x onerror="globalThis.__omXss=2">`;
const TARGET_BODY = 'Hostile id target message';
const REPLY_BODY = 'Hostile reply quoting the target';

async function collectCSPViolations(page) {
  await page.addInitScript(() => {
    window.__cspViolations = [];
    document.addEventListener('securitypolicyviolation', (event) => {
      window.__cspViolations.push(`${event.violatedDirective} ${event.blockedURI || ''} ${event.sample || ''}`.trim());
    });
  });
}

async function sarahConversationID(page) {
  return page.evaluate(async () => {
    const response = await fetch('/api/conversations?limit=200');
    const conversations = await response.json();
    return conversations.find(conversation => conversation.Name === 'Sarah Chen').ConversationID;
  });
}

// Serve Sarah's thread with two extra received messages: one whose own id is
// hostile, and a reply that quotes it.
async function injectHostileMessages(page, conversationID) {
  await page.route(new RegExp(`/api/conversations/${conversationID}/messages\\?`), async route => {
    const response = await route.fetch();
    const messages = await response.json();
    const template = messages.find(message => !message.IsFromMe) || messages[0];
    const now = Date.now();
    const target = {
      ...template,
      MessageID: HOSTILE_ID,
      Body: TARGET_BODY,
      TimestampMS: now - 1000,
      IsFromMe: false,
      MediaID: '',
      MimeType: '',
      Reactions: '',
      ReplyToID: '',
    };
    const reply = {
      ...target,
      MessageID: 'hostile-reply',
      Body: REPLY_BODY,
      TimestampMS: now,
      ReplyToID: HOSTILE_ID,
    };
    await route.fulfill({ response, json: [...messages, target, reply] });
  });
}

async function openSarah(page) {
  await page.locator('#conversation-list .convo-name').getByText('Sarah Chen', { exact: true }).first().click();
  await expect(page.locator('#chat-header-name')).toHaveText('Sarah Chen');
}

async function expectNothingRan(page) {
  expect(await page.evaluate(() => globalThis.__omXss)).toBeUndefined();
  await expect(page.locator('#messages-area img[src="x"]')).toHaveCount(0);
  expect(await page.evaluate(() => window.__cspViolations)).toEqual([]);
}

test.describe('sender-controlled ids', () => {
  test.beforeEach(async ({ page }) => {
    await collectCSPViolations(page);
    await page.goto('/');
    await injectHostileMessages(page, await sarahConversationID(page));
    await openSarah(page);
  });

  test('a reply quoting a hostile id renders as text and scrolls to the quoted message', async ({ page }) => {
    const reply = page.locator('#messages-area .msg').filter({ hasText: REPLY_BODY });
    const target = page.locator('#messages-area .msg').filter({ hasText: TARGET_BODY }).first();
    const preview = reply.locator('.msg-reply-preview');
    await expect(preview).toBeVisible();
    await expect(preview.locator('.reply-body')).toHaveText(TARGET_BODY);
    expect(await preview.getAttribute('data-reply-id')).toBe(HOSTILE_ID);
    expect(await preview.getAttribute('onclick')).toBeNull();
    expect(await target.getAttribute('data-msg-id')).toBe(HOSTILE_ID);

    await preview.click();
    // The handler got the raw id: it found the quoted message and outlined it.
    await expect.poll(() => target.evaluate(el => el.style.outline)).toContain('2px');
    await expectNothingRan(page);
  });

  test('quick and full-panel reactions on a hostile-id message send the raw id', async ({ page }) => {
    const reacted = [];
    await page.route('**/api/react', async route => {
      reacted.push(route.request().postDataJSON());
      await route.fulfill({ json: { success: true, state: 'confirmed' } });
    });
    const target = page.locator('#messages-area .msg').filter({ hasText: TARGET_BODY }).first();

    await target.hover();
    await target.locator('.action-react').click();
    const quick = target.locator('.emoji-picker button[data-emoji]').first();
    const quickEmoji = await quick.getAttribute('data-emoji');
    expect(await quick.getAttribute('onclick')).toBeNull();
    await quick.click();
    await expect.poll(() => reacted.length).toBe(1);
    expect(reacted[0]).toMatchObject({ message_id: HOSTILE_ID, emoji: quickEmoji, action: 'add' });

    await target.hover();
    await target.locator('.action-react').click();
    await target.locator('.emoji-plus').click();
    const panel = target.locator('.emoji-full-panel');
    await expect(panel).toHaveClass(/show/);
    const search = panel.locator('.emoji-search input');
    expect(await search.getAttribute('oninput')).toBeNull();
    await search.fill('fire');
    const gridButton = panel.locator('.emoji-grid button:visible').first();
    const gridEmoji = (await gridButton.textContent()).trim();
    await gridButton.click();
    await expect.poll(() => reacted.length).toBe(2);
    expect(reacted[1]).toMatchObject({ message_id: HOSTILE_ID, emoji: gridEmoji, action: 'add' });

    await expectNothingRan(page);
  });
});

test('the page script runs under a CSP that refuses inline handlers', async ({ page }) => {
  await collectCSPViolations(page);
  const response = await page.goto('/');
  const directives = Object.fromEntries(
    response.headers()['content-security-policy']
      .split(';')
      .map(part => part.trim().split(/\s+/))
      .filter(fields => fields[0])
      .map(([name, ...values]) => [name, values]),
  );
  expect(directives['script-src']).toContain("'self'");
  expect(directives['script-src']).not.toContain("'unsafe-inline'");
  expect(directives['script-src'].filter(value => value.startsWith("'sha256-"))).toHaveLength(1);
  expect(directives['script-src-attr']).toEqual(["'none'"]);

  // The hashed script ran: the inbox rendered.
  await expect(page.locator('#conversation-list .convo-item').first()).toBeVisible();
  await openSarah(page);
  await expect(page.locator('#messages-area .msg').first()).toBeVisible();

  // An injected handler is refused, as a missed escaping bug would be.
  await page.evaluate(() => {
    const probe = document.createElement('div');
    probe.id = 'csp-probe';
    probe.innerHTML = '<img src="data:image/gif;base64,R0lGODlhAQABAAAAACw=" onload="globalThis.__omXss=3">';
    document.body.appendChild(probe);
  });
  await expect.poll(() => page.evaluate(() => window.__cspViolations.length)).toBeGreaterThan(0);
  expect(await page.evaluate(() => globalThis.__omXss)).toBeUndefined();
  expect(await page.evaluate(() => window.__cspViolations.every(v => v.startsWith('script-src')))).toBe(true);
});

test.describe('without the control token', () => {
  test.use({ storageState: { cookies: [], origins: [] } });

  test('the API and MCP refuse the request and the page asks to authorize', async ({ page, request }) => {
    for (const path of ['/api/status', '/api/conversations?limit=5', '/api/v1/outbox/messages']) {
      const response = await request.get(path);
      expect(response.status(), path).toBe(401);
      expect(response.headers()['www-authenticate']).toBe('Bearer realm="openmessage"');
    }
    const send = await request.post('/api/send', { data: { conversation_id: 'conv1', message: 'should not send' } });
    expect(send.status()).toBe(401);
    const mcp = await request.post('/mcp', { data: { jsonrpc: '2.0', id: 1, method: 'tools/list' } });
    expect(mcp.status()).toBe(401);
    expect((await request.get('/healthz')).status()).toBe(200);

    await page.goto('/');
    await expect(page.locator('#auth-overlay')).toBeVisible();
    await expect(page.locator('#auth-overlay-error')).toBeHidden();
    await expect(page.locator('#conversation-list .convo-item')).toHaveCount(0);
  });

  test('pasting the control token authorizes this browser', async ({ page }) => {
    await page.goto('/');
    const overlay = page.locator('#auth-overlay');
    await expect(overlay).toBeVisible();

    await overlay.locator('input[name="token"]').fill('0'.repeat(64));
    await overlay.locator('button[type="submit"]').click();
    await expect(page.locator('#auth-overlay-error')).toBeVisible();
    expect(new URL(page.url()).search).toBe('');

    await overlay.locator('input[name="token"]').fill(controlToken);
    await overlay.locator('button[type="submit"]').click();
    await expect(page.locator('#conversation-list .convo-item').first()).toBeVisible();
    await expect(overlay).toBeHidden();
    const status = await page.evaluate(async () => (await fetch('/api/status')).status);
    expect(status).toBe(200);
  });
});
