const { test, expect } = require('@playwright/test');

const hikingDraft = 'Count me in for Saturday! Lands End trail looks clear — 62°F and sunny. Want me to bring snacks?';

async function openConversation(page, name) {
  await page.locator('#conversation-list .convo-name').getByText(name, { exact: true }).first().click();
  await expect(page.locator('#chat-header-name')).toHaveText(name);
}

async function openPlatforms(page) {
  await page.keyboard.press('Control+,');
  await expect(page.locator('#wa-overlay.show h2')).toHaveText('Platforms');
}

async function expectThreadNearBottom(page) {
  await expect
    .poll(async () => page.locator('#messages-area').evaluate((el) => {
      return el.scrollHeight - el.scrollTop - el.clientHeight;
    }))
    .toBeLessThan(8);
}

async function expectLastMessageVisible(page) {
  await expect
    .poll(async () => page.locator('#messages-area').evaluate((container) => {
      const messages = container.querySelectorAll('.msg');
      const last = messages[messages.length - 1];
      if (!last) return false;
      const containerRect = container.getBoundingClientRect();
      const lastRect = last.getBoundingClientRect();
      return lastRect.bottom <= containerRect.bottom && lastRect.top >= containerRect.top;
    }))
    .toBe(true);
}

async function installFakeNotifications(page) {
  await page.evaluate(() => {
    const created = [];
    function FakeNotification(title, options = {}) {
      created.push({ title, body: options.body || '', tag: options.tag || '' });
      this.onclick = null;
    }
    FakeNotification.permission = 'default';
    FakeNotification.requestPermission = async () => {
      FakeNotification.permission = 'granted';
      return 'granted';
    };
    window.__openMessageNotifications = created;
    window.Notification = FakeNotification;
  });
}

test.beforeEach(async ({ page, request }) => {
  await request.post('/_e2e/drafts', {
    data: {
      body: hikingDraft,
      conversation_id: 'conv1',
      draft_id: 'draft1',
    },
  });
  await page.goto('/');
});

test('loads the seeded conversation list and thread view', async ({ page }) => {
  await expect(page.locator('#connection-banner.disconnected')).toHaveCount(0);
  await expect
    .poll(async () => page.locator('#conversation-list .convo-item').count())
    .toBeGreaterThan(10);
  await openConversation(page, 'Sarah Chen');
  await expect(page.locator('#messages-area')).toContainText('Hey! Are you free for dinner tonight?');
  await expect(page.locator('#compose-input')).toBeVisible();
});

test('opens a deep-linked conversation from the URL', async ({ page }) => {
  await page.goto('/?conversation=conv1');
  await expect(page.locator('#chat-header-name')).toHaveText('Sarah Chen');
  await expect(page.locator('#messages-area')).toContainText('Hey! Are you free for dinner tonight?');
});

test('shows platform badges and filters threads by source', async ({ page }) => {
  await expect(page.locator('#sidebar-source-filters')).toContainText('WhatsApp');
  await expect(page.locator('#sidebar-source-filters')).toContainText('Signal');
  await expect(page.getByRole('button', { name: /WhatsApp 6/i })).toBeVisible();

  await page.getByRole('button', { name: /WhatsApp 6/i }).click();

  await expect(page.locator('#conversation-list .convo-item')).toHaveCount(6);
  await expect(page.locator('#conversation-list')).toContainText('Weekend Hiking Group');
  await expect(page.locator('#conversation-list')).toContainText('Lisa Rodriguez');
  await expect(page.locator('#conversation-list')).toContainText('Jordan Rivera');
  await expect(page.locator('#conversation-list')).not.toContainText('Sarah Chen');

  await openConversation(page, 'Weekend Hiking Group');
  await expect(page.locator('#chat-header-source')).toContainText('WhatsApp');
});

test('opens the platforms screen from the empty-state onboarding CTA', async ({ page }) => {
  await page.route('**/api/status', async route => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        connected: false,
        google: { connected: false, paired: false, needs_pairing: true },
        whatsapp: { connected: false, paired: false, pairing: false, qr_available: false },
        signal: { connected: false, paired: false, pairing: false, qr_available: false },
      }),
    });
  });
  await page.route('**/api/conversations?limit=200', async route => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: '[]',
    });
  });

  await page.reload();
  await expect(page.locator('#empty-state-title')).toHaveText('Add your first platform');
  await expect(page.locator('#empty-state-cta')).toHaveText('Add platform');
  await page.locator('#empty-state-cta').click();

  await expect(page.locator('#wa-overlay.show h2')).toHaveText('Platforms');
  await expect(page.locator('#wa-overlay')).toContainText('Google Messages');
  await expect(page.locator('#wa-overlay')).toContainText('WhatsApp');
  await expect(page.locator('#wa-overlay')).toContainText('Signal');
  await expect(page.locator('#signal-history-note')).toContainText('Transfer Message History');
});

test('shows live Signal history import progress after pairing starts', async ({ page }) => {
  await page.route('**/api/signal/status', async route => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        connected: true,
        paired: true,
        history_sync: {
          running: true,
          started_at: 1700000000000,
          imported_conversations: 3,
          imported_messages: 42,
        },
      }),
    });
  });

  await openPlatforms(page);

  await expect(page.locator('#signal-status-pill')).toHaveText('Importing history');
  await expect(page.locator('#signal-helper')).toContainText('still importing older history');
  await expect(page.locator('#signal-history-note')).toContainText('3 chats');
  await expect(page.locator('#signal-history-note')).toContainText('42 messages');
});

test('offers a direct Google pairing handoff when Google Messages is unpaired', async ({ page }) => {
  await page.addInitScript(() => {
    window.__openMessageClipboard = '';
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        writeText: async (text) => {
          window.__openMessageClipboard = text;
        },
      },
    });
  });
  await page.route('**/api/status', async route => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        connected: true,
        google: {
          connected: false,
          paired: false,
          needs_pairing: true,
        },
        whatsapp: {
          connected: true,
          paired: true,
        },
        signal: {
          connected: true,
          paired: true,
        },
      }),
    });
  });

  await page.reload();
  await openPlatforms(page);
  await expect(page.locator('#gm-reconnect-btn')).toHaveText('Copy pair command');
  await page.locator('#gm-reconnect-btn').click();

  await expect.poll(async () => page.evaluate(() => window.__openMessageClipboard)).toBe('openmessage pair');
  await expect(page.locator('#thread-feedback')).toContainText('Pair command copied.');
});

test('opens a Signal group with slashes in its conversation id', async ({ page }) => {
  await openConversation(page, 'Strategy Lab');

  await expect(page.locator('#chat-header-name')).toHaveText('Strategy Lab');
  await expect(page.locator('#messages-area')).toContainText('recent logistics outage');
  await expect(page.locator('#messages-area')).not.toContainText('Signal is easier for me if you want to reply here.');
});

test('lets you scroll the platforms screen when pairing cards expand', async ({ page }) => {
  const qrDataUrl = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9s1qN9sAAAAASUVORK5CYII=';

  await page.setViewportSize({ width: 1280, height: 620 });
  await page.route('**/api/whatsapp/status', async route => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        connected: false,
        paired: false,
        pairing: true,
        qr_available: true,
      }),
    });
  });
  await page.route('**/api/signal/status', async route => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        connected: false,
        paired: false,
        pairing: true,
        qr_available: true,
      }),
    });
  });
  await page.route('**/api/whatsapp/qr', async route => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        png_data_url: qrDataUrl,
        expires_at: Date.now() + 60_000,
      }),
    });
  });
  await page.route('**/api/signal/qr', async route => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        png_data_url: qrDataUrl,
      }),
    });
  });

  await page.reload();
  await openPlatforms(page);

  const modalBody = page.locator('#wa-overlay .wa-body');
  await expect
    .poll(async () => modalBody.evaluate((el) => el.scrollHeight > el.clientHeight))
    .toBe(true);

  await modalBody.evaluate((el) => {
    el.scrollTop = el.scrollHeight;
  });

  await expect
    .poll(async () => modalBody.evaluate((el) => el.scrollTop))
    .toBeGreaterThan(30);
});

test('lets you leave a WhatsApp group from the thread header', async ({ page }) => {
  page.on('dialog', (dialog) => dialog.accept());

  await page.getByRole('button', { name: /WhatsApp 6/i }).click();
  await openConversation(page, 'Weekend Hiking Group');

  await expect(page.locator('#chat-header-leave-group-btn')).toBeVisible();
  await page.locator('#chat-header-leave-group-btn').click();

  await expect(page.locator('#thread-feedback')).toContainText('Left WhatsApp group.');
  await expect(page.locator('#conversation-list')).not.toContainText('Weekend Hiking Group');
  await expect(page.getByRole('button', { name: /WhatsApp 5/i })).toBeVisible();
});

test('search matches conversation names and updates platform chip counts', async ({ page }) => {
  await page.locator('#search-input').fill('Jordan');

  await expect(page.locator('#conversation-list .convo-item')).toHaveCount(4);
  await expect(page.locator('#sidebar-source-filters')).toContainText('All');
  await expect(page.locator('#sidebar-source-filters')).toContainText('SMS');
  await expect(page.locator('#sidebar-source-filters')).toContainText('WhatsApp');
  await expect(page.getByRole('button', { name: /All 4/i })).toBeVisible();
  await expect(page.getByRole('button', { name: /SMS 2/i })).toBeVisible();
  await expect(page.getByRole('button', { name: /WhatsApp 2/i })).toBeVisible();

  await page.getByRole('button', { name: /WhatsApp 2/i }).click();
  await expect(page.locator('#conversation-list .convo-item')).toHaveCount(2);
  await expect(page.locator('#conversation-list')).toContainText('Jordan Rivera');
});

test('renders clickable links with a social preview card', async ({ page, request }) => {
  await openConversation(page, 'Sarah Chen');

  await request.post('/_e2e/messages', {
    data: {
      body: 'Read this example.com/story',
      conversation_id: 'conv1',
      sender_name: 'Sarah Chen',
      sender_number: '+14155551234',
    },
  });

  const linkedMessage = page.locator('#messages-area .msg').filter({ hasText: 'example.com/story' }).last();
  await expect(linkedMessage.locator('a.msg-link')).toHaveAttribute('href', 'https://example.com/story');
  await expect(linkedMessage.locator('.msg-link-preview')).toContainText('Example Story');
  await expect(linkedMessage.locator('.msg-link-preview')).toContainText('Example');
});

test('coalesces duplicate direct chats into one sidebar row with route tabs in the thread header', async ({ page }) => {
  const jordanRows = page.locator('#conversation-list > .convo-item').filter({
    has: page.locator('.convo-name').getByText('Jordan Rivera', { exact: true }),
  });

  await expect(jordanRows).toHaveCount(3);
  await expect(jordanRows.first().locator('.convo-avatar .avatar-platform-stack .platform-chip')).toHaveCount(2);

  await jordanRows.first().click();
  await expect(page.locator('#chat-header-name')).toHaveText('Jordan Rivera');
  await expect(page.locator('#chat-header-source .chat-route-tab')).toHaveCount(2);
  await expect(page.locator('#chat-header-source')).toContainText('WhatsApp');
  await expect(page.locator('#chat-header-source')).toContainText('SMS');
});

test('shows platform markers on avatars instead of direct-row text pills', async ({ page }) => {
  const sarahRow = page.locator('#conversation-list > .convo-item').filter({ hasText: 'Sarah Chen' }).first();

  await expect(sarahRow.locator('.convo-avatar .avatar-platform-stack .platform-chip')).toHaveCount(1);
  await expect(sarahRow.locator('.convo-subline .platform-chip')).toHaveCount(0);
});

test('switches routes from the thread header tabs while keeping the sidebar person-first', async ({ page }) => {
  const jordanRows = page.locator('#conversation-list > .convo-item').filter({
    has: page.locator('.convo-name').getByText('Jordan Rivera', { exact: true }),
  });

  await jordanRows.first().click();
  await expect(page.locator('#compose-input')).toBeEnabled();
  await expect(page.locator('#chat-header-source .chat-route-tab.active')).toContainText('WhatsApp');
  await expect(page.locator('#chat-pane')).not.toContainText('Sending via WhatsApp');
  await expect(page.locator('#chat-pane')).not.toContainText('Replies here');
  await expect(page.locator('#chat-pane')).not.toContainText('Reply route');

  await page.locator('#chat-header-source .chat-route-tab').filter({ hasText: 'SMS' }).click();
  await expect(page.locator('#chat-header-source .chat-route-tab.active')).toContainText('SMS');
  await expect(page.locator('#compose-input')).toBeEnabled();
});

test('does not group same-name chats when participant identifiers differ', async ({ page }) => {
  await expect(
    page.locator('#conversation-list > .convo-item').filter({
      has: page.locator('.convo-name').getByText('Jordan Rivera', { exact: true }),
    })
  ).toHaveCount(3);
});

test('new message surfaces existing routes for the same number', async ({ page }) => {
  await page.locator('#new-msg-btn').click();
  await page.locator('#new-msg-phone').fill('+1 (415) 555-0199');

  await expect(page.locator('#new-msg-helper')).toContainText('Choose the route you want');
  await expect(page.locator('#new-msg-routes')).toContainText('SMS');
  await expect(page.locator('#new-msg-routes')).toContainText('WhatsApp');
  await expect(page.locator('#new-msg-go')).toContainText('Open SMS route');
});

test('sends a message through the compose box', async ({ page }) => {
  const outbound = `Playwright outbound ${Date.now()}`;

  await openConversation(page, 'Sarah Chen');
  await page.locator('#compose-input').fill(outbound);
  await page.locator('#send-btn').click();

  await expect(page.locator('#messages-area')).toContainText(outbound);
});

test('sends a Signal text message through the compose box', async ({ page }) => {
  const outbound = `Signal outbound ${Date.now()}`;

  await openConversation(page, 'Taylor Price');
  await expect(page.locator('#chat-header-source')).toContainText('Signal');
  await page.locator('#compose-input').fill(outbound);
  await page.locator('#send-btn').click();

  await expect(page.locator('#messages-area')).toContainText(outbound);
});

test('sends a Signal image attachment through the compose box', async ({ page }) => {
  await openConversation(page, 'Taylor Price');
  await expect(page.locator('#chat-header-source')).toContainText('Signal');
  await page.locator('#file-input').setInputFiles({
    name: 'signal-photo.png',
    mimeType: 'image/png',
    buffer: Buffer.from(
      'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO7Z9wAAAABJRU5ErkJggg==',
      'base64',
    ),
  });

  await expect(page.locator('#attach-preview')).toHaveClass(/active/);
  await page.locator('#send-btn').click();

  await expect(page.locator('#attach-preview')).not.toHaveClass(/active/);
  await expect(page.locator('#messages-area img[src*="/api/media/"]').last()).toBeVisible();
});

test('keeps the active thread pinned to the bottom after sending', async ({ page }) => {
  const outbound = `Bottom send ${Date.now()}`;

  await openConversation(page, 'Paged Thread');
  await expect(page.locator('#messages-area .msg')).toHaveCount(100);
  await expectThreadNearBottom(page);

  await page.locator('#compose-input').fill(outbound);
  await page.locator('#send-btn').click();

  await expect(page.locator('#messages-area')).toContainText(outbound);
  await expectThreadNearBottom(page);
  await expectLastMessageVisible(page);
});

test('sends a WhatsApp text message through the compose box', async ({ page }) => {
  const outbound = `WhatsApp outbound ${Date.now()}`;
  const jordanRow = page.locator('#conversation-list > .convo-item').filter({
    has: page.locator('.convo-name').getByText('Jordan Rivera', { exact: true }),
  }).first();

  await jordanRow.click();
  await expect(page.locator('#chat-header-source .chat-route-tab.active')).toContainText('WhatsApp');
  await page.locator('#compose-input').fill(outbound);
  await page.locator('#send-btn').click();

  await expect(page.locator('#messages-area')).toContainText(outbound);
});

test('renders read receipts for read messages', async ({ page, request }) => {
  const outbound = `Read receipt ${Date.now()}`;

  await openConversation(page, 'Sarah Chen');
  await request.post('/_e2e/messages', {
    data: {
      body: outbound,
      conversation_id: 'conv1',
      is_from_me: true,
      sender_name: 'Me',
      sender_number: '+14155550000',
      status: 'READ',
    },
  });

  const sentMessage = page.locator('#messages-area .msg.sent').filter({ hasText: outbound }).last();
  await expect(sentMessage.locator('.msg-status.status-read')).toBeVisible();
});

test('rerenders when an older visible message changes outside the former tail window', async ({ page }) => {
  await openConversation(page, 'Paged Thread');

  const targetMessage = page.locator('#messages-area .msg[data-msg-id="paged-150"]').first();
  await expect(targetMessage).toContainText('Paged message 150');
  await expect(targetMessage.locator('.reaction-pill')).toHaveCount(0);

  await page.evaluate(() => {
    if (!window.__openMessageTestHooks.updateLoadedMessage('paged-150', {
      Reactions: JSON.stringify([{ emoji: '🔥', count: 1 }]),
    })) {
      throw new Error('paged-150 not found in loadedMessages');
    }
    window.__openMessageTestHooks.renderLoadedMessages();
  });

  await expect(targetMessage.locator('.reaction-pill')).toContainText('🔥');
});

test('ignores stale avatar fetches when reusing an avatar node', async ({ page }) => {
  await page.evaluate(() => {
    window.__avatarHydrationTest = { pending: [] };
    window.__openMessageTestHooks.installAvatarFetchOverride((...args) => new Promise((resolve) => {
      window.__avatarHydrationTest.pending.push({ args, resolve });
    }));

    const host = document.createElement('div');
    host.id = 'e2e-avatar-host';
    host.className = 'chat-header-avatar';
    document.body.appendChild(host);

    window.__openMessageTestHooks.renderAvatarElement(host, {
      name: 'Sarah Chen',
      numbers: ['+14155551234'],
      text: 'S',
      background: '#111827',
    });
    window.__openMessageTestHooks.renderAvatarElement(host, {
      name: 'Marcus Johnson',
      numbers: ['+12125559876'],
      text: 'M',
      background: '#1f2937',
    });
  });

  await expect.poll(async () => page.evaluate(() => window.__avatarHydrationTest.pending.length)).toBe(2);

  await page.evaluate(() => {
    window.__avatarHydrationTest.pending[0].resolve('data:image/gif;base64,R0lGODlhAQABAAAAACwAAAAAAQABAAA=');
  });
  await expect
    .poll(async () => page.evaluate(() => document.querySelector('#e2e-avatar-host img')?.alt || ''))
    .not.toBe('Sarah Chen photo');

  await page.evaluate(() => {
    window.__avatarHydrationTest.pending[1].resolve('data:image/gif;base64,R0lGODlhAQABAAAAACwAAAAAAQABAAA=');
  });
  await expect
    .poll(async () => page.evaluate(() => document.querySelector('#e2e-avatar-host img')?.alt || ''))
    .toBe('Marcus Johnson photo');
});

test('hydrates the full emoji grid only when requested', async ({ page }) => {
  await openConversation(page, 'Sarah Chen');

  const firstMessage = page.locator('#messages-area .msg').first();
  await expect(firstMessage.locator('.emoji-full-panel')).not.toHaveAttribute('data-hydrated', 'true');
  await expect(firstMessage.locator('.emoji-full-panel [data-emoji]')).toHaveCount(0);

  await page.evaluate(() => {
    const message = document.querySelector('#messages-area .msg');
    if (!message) throw new Error('message not found');
    const messageId = message.getAttribute('data-msg-id');
    if (!messageId) throw new Error('message id missing');
    window.toggleFullEmojiPanel({ stopPropagation() {} }, messageId);
  });

  await expect(firstMessage.locator('.emoji-full-panel')).toHaveAttribute('data-hydrated', 'true');
  await expect
    .poll(async () => firstMessage.locator('.emoji-full-panel [data-emoji]').count())
    .toBeGreaterThan(50);
});

test('sends a WhatsApp image attachment through the compose box', async ({ page }) => {
  const jordanRow = page.locator('#conversation-list > .convo-item').filter({
    has: page.locator('.convo-name').getByText('Jordan Rivera', { exact: true }),
  }).first();

  await jordanRow.click();
  await expect(page.locator('#chat-header-source .chat-route-tab.active')).toContainText('WhatsApp');
  await page.locator('#file-input').setInputFiles({
    name: 'wa-photo.png',
    mimeType: 'image/png',
    buffer: Buffer.from(
      'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO7Z9wAAAABJRU5ErkJggg==',
      'base64',
    ),
  });

  await expect(page.locator('#attach-preview')).toHaveClass(/active/);
  await page.locator('#send-btn').click();

  await expect(page.locator('#attach-preview')).not.toHaveClass(/active/);
  await expect(page.locator('#messages-area img[src*="/api/media/"]').last()).toBeVisible();
});

test('sends a WhatsApp voice note attachment through the compose box', async ({ page }) => {
  const jordanRow = page.locator('#conversation-list > .convo-item').filter({
    has: page.locator('.convo-name').getByText('Jordan Rivera', { exact: true }),
  }).first();

  await jordanRow.click();
  await expect(page.locator('#chat-header-source .chat-route-tab.active')).toContainText('WhatsApp');
  await page.locator('#file-input').setInputFiles({
    name: 'voice-note.ogg',
    mimeType: 'audio/ogg',
    buffer: Buffer.from('T2dnUwACAAAAAAAAAABVDxQRAAAAAF7kK3UBHgF2b3JiaXMAAAAAAUSsAAAAAAAAgDgAAAAAAAC4AU9nZ1MAAAAAAAAAAAAAFQ8UEQEAAABxvLC3DkD/////////////////AHZvcmJpczQAAABYaXBoLk9yZwAAAAAAAAAAAAAA', 'base64'),
  });

  await expect(page.locator('#attach-preview')).toHaveClass(/active/);
  await page.locator('#send-btn').click();

  await expect(page.locator('#attach-preview')).not.toHaveClass(/active/);
  await expect(page.locator('#messages-area audio[src*="/api/media/"]').last()).toBeVisible();
});

test('restores compose text when send fails', async ({ page }) => {
  const outbound = `Send failure ${Date.now()}`;

  await openConversation(page, 'Sarah Chen');
  await page.route('**/api/send', route => route.fulfill({
    status: 500,
    contentType: 'text/plain',
    body: 'local persistence failed',
  }), { times: 1 });

  await page.locator('#compose-input').fill(outbound);
  await page.locator('#send-btn').click();

  await expect(page.locator('#thread-feedback')).toContainText('local persistence failed');
  await expect(page.locator('#compose-input')).toHaveValue(outbound);
  await expect(page.locator('#messages-area')).not.toContainText(outbound);
});

test('refreshes the active thread from SSE invalidations', async ({ page, request }) => {
  const inbound = `SSE inbound ${Date.now()}`;

  await openConversation(page, 'Sarah Chen');
  await request.post('/_e2e/messages', {
    data: {
      body: inbound,
      conversation_id: 'conv1',
      sender_name: 'Sarah Chen',
      sender_number: '+14155551234',
    },
  });

  await expect(page.locator('#messages-area')).toContainText(inbound);
});

test('keeps the active thread pinned to the bottom for inbound live messages', async ({ page, request }) => {
  const inbound = `Bottom inbound ${Date.now()}`;

  await openConversation(page, 'Paged Thread');
  await expect(page.locator('#messages-area .msg')).toHaveCount(100);
  await expectThreadNearBottom(page);

  await request.post('/_e2e/messages', {
    data: {
      body: inbound,
      conversation_id: 'conv-paged',
      sender_name: 'Pat Page',
      sender_number: '+15550000001',
    },
  });

  await expect(page.locator('#messages-area')).toContainText(inbound);
  await expectThreadNearBottom(page);
  await expectLastMessageVisible(page);
});

test('keeps the newest message visible when its bubble grows after render', async ({ page, request }) => {
  const inbound = `Delayed growth ${Date.now()}`;

  await openConversation(page, 'Paged Thread');
  await expect(page.locator('#messages-area .msg')).toHaveCount(100);
  await expectThreadNearBottom(page);

  await request.post('/_e2e/messages', {
    data: {
      body: inbound,
      conversation_id: 'conv-paged',
      sender_name: 'Pat Page',
      sender_number: '+15550000001',
    },
  });

  const newestMessage = page.locator('#messages-area .msg').filter({ hasText: inbound }).last();
  await expect(newestMessage).toBeVisible();
  await expectLastMessageVisible(page);

  await newestMessage.evaluate((node) => {
    const filler = document.createElement('div');
    filler.className = 'e2e-growth-filler';
    filler.style.height = '280px';
    filler.style.marginTop = '12px';
    filler.textContent = 'Expanded after render';
    node.appendChild(filler);
  });

  await expectLastMessageVisible(page);
  await expectThreadNearBottom(page);
});

test('keeps the newest message visible when the thread viewport shrinks', async ({ page }) => {
  await openConversation(page, 'Paged Thread');
  await expect(page.locator('#messages-area .msg')).toHaveCount(100);
  await expectThreadNearBottom(page);
  await expectLastMessageVisible(page);

  await page.locator('#compose-bar').evaluate((el) => {
    el.style.paddingBottom = '260px';
  });

  await expectLastMessageVisible(page);
  await expectThreadNearBottom(page);
});

test('stays pinned when a transient scroll event fires during late message growth', async ({ page }) => {
  await openConversation(page, 'Paged Thread');
  await expect(page.locator('#messages-area .msg')).toHaveCount(100);
  await expectThreadNearBottom(page);
  await expectLastMessageVisible(page);

  await page.evaluate(() => {
    const area = document.getElementById('messages-area');
    const messages = area?.querySelectorAll('.msg');
    const anchorMessage = messages && messages.length > 1 ? messages[messages.length - 2] : null;
    if (!area || !anchorMessage) return;

    const filler = document.createElement('div');
    filler.className = 'e2e-growth-filler';
    filler.style.height = '220px';
    filler.style.marginTop = '12px';
    filler.textContent = 'Late growth after render';
    anchorMessage.appendChild(filler);

    area.dispatchEvent(new Event('scroll'));
  });

  await expectLastMessageVisible(page);
  await expectThreadNearBottom(page);
});

test('shows and clears a typing indicator from live typing events', async ({ page, request }) => {
  await openConversation(page, 'Sarah Chen');

  await request.post('/_e2e/typing', {
    data: {
      conversation_id: 'conv1',
      sender_name: 'Sarah Chen',
      sender_number: '+14155551234',
      typing: true,
    },
  });

  await expect(page.locator('#chat-header-status')).toHaveText('Typing...');
  await expect(page.locator('#chat-header-status')).toHaveClass(/typing/);

  await request.post('/_e2e/typing', {
    data: {
      conversation_id: 'conv1',
      sender_name: 'Sarah Chen',
      sender_number: '+14155551234',
      typing: false,
    },
  });

  await expect(page.locator('#chat-header-status')).not.toHaveText('Typing...');
});

test('shows a desktop notification for an unseen inbound message when enabled', async ({ page, request }) => {
  const inbound = `Notification inbound ${Date.now()}`;

  await installFakeNotifications(page);

  await page.locator('#notif-btn').click();
  await expect(page.locator('#notif-btn')).toHaveClass(/active/);

  await request.post('/_e2e/messages', {
    data: {
      body: inbound,
      conversation_id: 'conv2',
      sender_name: 'Marcus Johnson',
      sender_number: '+12125559876',
    },
  });

  await expect.poll(async () => page.evaluate(() => window.__openMessageNotifications.length)).toBe(1);
  await expect.poll(async () => page.evaluate(() => window.__openMessageNotifications[0]?.title || '')).toBe('Marcus Johnson');
  await expect.poll(async () => page.evaluate(() => window.__openMessageNotifications[0]?.body || '')).toBe(inbound);
});

test('suppresses desktop notifications for the active visible thread', async ({ page, request }) => {
  const inbound = `Focused thread ${Date.now()}`;

  await installFakeNotifications(page);

  await page.locator('#notif-btn').click();
  await openConversation(page, 'Sarah Chen');

  await request.post('/_e2e/messages', {
    data: {
      body: inbound,
      conversation_id: 'conv1',
      sender_name: 'Sarah Chen',
      sender_number: '+14155551234',
    },
  });

  await expect(page.locator('#messages-area')).toContainText(inbound);
  await expect.poll(async () => page.evaluate(() => window.__openMessageNotifications.length)).toBe(0);
});

test('lets you mute a thread from the header notification menu', async ({ page, request }) => {
  const inbound = `Muted inbound ${Date.now()}`;

  await installFakeNotifications(page);
  await page.locator('#notif-btn').click();
  await openConversation(page, 'Sarah Chen');

  await page.locator('#chat-header-notification-btn').click();
  await page.locator('#context-menu .context-menu-item').getByText('Mute notifications', { exact: true }).click();

  await expect(page.locator('#thread-feedback')).toContainText('Notifications set to muted.');
  await expect(page.locator('#conversation-list .convo-item').filter({ hasText: 'Sarah Chen' }).locator('.notification-mode-badge')).toBeVisible();

  await request.post('/_e2e/messages', {
    data: {
      body: inbound,
      conversation_id: 'conv1',
      sender_name: 'Sarah Chen',
      sender_number: '+14155551234',
    },
  });

  await expect(page.locator('#messages-area')).toContainText(inbound);
  await expect.poll(async () => page.evaluate(() => window.__openMessageNotifications.length)).toBe(0);
});

test('supports mentions-only notifications from the conversation context menu', async ({ page, request }) => {
  const mutedInbound = `Checking in ${Date.now()}`;
  const mentionInbound = `Checking in ${Date.now()}`;
  const marcusRow = page.locator('#conversation-list .convo-item').filter({ hasText: 'Marcus Johnson' }).first();

  await installFakeNotifications(page);
  await page.locator('#notif-btn').click();

  await marcusRow.click({ button: 'right' });
  await page.locator('#context-menu .context-menu-item').getByText('Notify on mentions only', { exact: true }).click();

  await expect(page.locator('#thread-feedback')).toContainText('Notifications set to mentions.');
  await expect(marcusRow.locator('.notification-mode-badge')).toContainText('@');

  await request.post('/_e2e/messages', {
    data: {
      body: mutedInbound,
      conversation_id: 'conv2',
      sender_name: 'Marcus Johnson',
      sender_number: '+12125559876',
    },
  });

  await expect.poll(async () => page.evaluate(() => window.__openMessageNotifications.length)).toBe(0);

  await request.post('/_e2e/messages', {
    data: {
      body: mentionInbound,
      conversation_id: 'conv2',
      mentions_me: true,
      sender_name: 'Marcus Johnson',
      sender_number: '+12125559876',
    },
  });

  await expect.poll(async () => page.evaluate(() => window.__openMessageNotifications.length)).toBe(1);
  await expect.poll(async () => page.evaluate(() => window.__openMessageNotifications[0]?.body || '')).toBe(mentionInbound);
});

test('supports message right-click actions', async ({ page }) => {
  await openConversation(page, 'Sarah Chen');

  const targetMessage = page.locator('#messages-area .msg.received').filter({ hasText: 'Hey! Are you free for dinner tonight?' }).first();
  await targetMessage.click({ button: 'right' });
  await expect(page.locator('#context-menu .context-menu-item').getByText('Reply', { exact: true })).toBeVisible();
  await page.locator('#context-menu .context-menu-item').getByText('Copy text', { exact: true }).click();

  await expect(page.locator('#thread-feedback')).toContainText('Message copied.');

  await targetMessage.click({ button: 'right' });
  await page.locator('#context-menu .context-menu-item').getByText('Reply', { exact: true }).click();
  await expect(page.locator('#reply-indicator')).toHaveClass(/show/);
  await expect(page.locator('#reply-to-text')).toContainText('Hey! Are you free for dinner tonight?');
});

test('preserves draft edits during SSE refresh', async ({ page, request }) => {
  const editedDraft = `Edited draft ${Date.now()}`;
  const inbound = `Draft refresh ${Date.now()}`;

  await openConversation(page, 'Sarah Chen');
  await page.locator('.draft-text').fill(editedDraft);

  await request.post('/_e2e/messages', {
    data: {
      body: inbound,
      conversation_id: 'conv1',
      sender_name: 'Sarah Chen',
      sender_number: '+14155551234',
    },
  });

  await expect(page.locator('#messages-area')).toContainText(inbound);
  await expect(page.locator('.draft-text')).toHaveValue(editedDraft);
});

test('loads older pages when scrolling up in a long thread', async ({ page }) => {
  await openConversation(page, 'Paged Thread');
  await expect(page.locator('#messages-area .msg')).toHaveCount(100);
  await expect(page.locator('#messages-area')).not.toContainText('Paged message 001');

  await page.locator('#messages-area').evaluate((el) => {
    el.scrollTop = 0;
    el.dispatchEvent(new Event('scroll'));
  });

  await expect
    .poll(async () => page.evaluate(() => window.__openMessageTestHooks.threadRenderStats().loadedMessages))
    .toBeGreaterThanOrEqual(150);
  await expect
    .poll(async () => page.evaluate(() => {
      const stats = window.__openMessageTestHooks.threadRenderStats();
      return stats.renderedMessages <= stats.windowSize;
    }))
    .toBe(true);

  await page.locator('#messages-area').evaluate((el) => {
    el.scrollTop = 0;
    el.dispatchEvent(new Event('scroll'));
  });

  await expect(page.locator('#messages-area')).toContainText('Paged message 001');
  await expect
    .poll(async () => page.evaluate(() => {
      const stats = window.__openMessageTestHooks.threadRenderStats();
      return stats.renderedMessages <= stats.windowSize;
    }))
    .toBe(true);
});

test('sends an AI draft from the thread banner', async ({ page }) => {
  const draftReply = `Draft sent ${Date.now()}`;

  await openConversation(page, 'Sarah Chen');
  await expect(page.locator('.draft-banner')).toHaveCount(1);
  await page.locator('.draft-text').fill(draftReply);
  await page.locator('.draft-send-btn').click();

  await expect(page.locator('.draft-banner')).toHaveCount(0);
  await expect(page.locator('#messages-area')).toContainText(draftReply);
});

test('discards an AI draft from the thread banner', async ({ page }) => {
  await openConversation(page, 'Sarah Chen');
  await expect(page.locator('.draft-banner')).toHaveCount(1);
  await page.locator('.draft-discard-btn').click();
  await expect(page.locator('.draft-banner')).toHaveCount(0);
});
