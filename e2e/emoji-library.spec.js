const { test, expect } = require('@playwright/test');

async function openThread(page) {
  await page.goto('/');
  await page.locator('#conversation-list .convo-name').getByText('Sarah Chen', { exact: true }).first().click();
  await expect(page.locator('#chat-header-name')).toHaveText('Sarah Chen');
}

test('searches common emoji and inserts a complete facepalm sequence at the cursor', async ({ page }) => {
  await openThread(page);
  const input = page.locator('#compose-input');
  await input.fill('Before after');
  await input.evaluate(el => el.setSelectionRange(7, 7));
  await page.locator('#compose-emoji-btn').click();

  const search = page.locator('#compose-emoji-search');
  const grid = page.locator('#compose-emoji-grid');
  for (const [query, emoji] of [
    ['facepalm', '🤦'], ['face palm', '🤦🏽‍♀️'], ['smh', '🤦‍♂️'],
    ['shrug', '🤷'], ['idk', '🤷🏿'], ['melting', '🫠'],
    ['pink heart', '🩷'], ['phoenix', '🐦‍🔥'], ['eyes', '👀'],
    ['party', '🎉'], ['usa', '🇺🇸'],
  ]) {
    await search.fill(query);
    await expect(grid.locator(`button[data-emoji="${emoji}"]`)).toBeVisible();
  }
  await search.fill('no-such-emoji');
  await expect(grid.locator('button:visible')).toHaveCount(0);
  await expect(grid.locator('.emoji-cat-label:visible')).toHaveCount(0);

  await search.fill('facepalm');
  await grid.locator('button[data-emoji="🤦🏽‍♀️"]').click();
  await expect(input).toHaveValue('Before 🤦🏽‍♀️after');
  await expect(input).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(page.locator('#compose-emoji-panel')).not.toHaveClass(/show/);
});

test('offers named emoji without duplicates in a narrow compose picker', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await openThread(page);
  await expect(page.locator('#compose-emoji-grid button')).toHaveCount(0);
  await page.locator('#compose-emoji-btn').click();
  const catalog = await page.locator('#compose-emoji-grid').evaluate(grid => {
    const buttons = Array.from(grid.querySelectorAll('button'));
    return {
      count: buttons.length,
      unique: new Set(buttons.map(button => button.dataset.emoji)).size,
      named: buttons.every(button => button.dataset.name && button.getAttribute('aria-label') !== 'emoji'),
      fits: grid.scrollWidth <= grid.clientWidth + 1,
    };
  });
  expect(catalog.count).toBe(3944);
  expect(catalog.unique).toBe(catalog.count);
  expect(catalog.named).toBe(true);
  expect(catalog.fits).toBe(true);
});

test('sends the selected facepalm skin-tone variant as a reaction', async ({ page }) => {
  await openThread(page);
  let reaction;
  await page.route('**/api/react', async route => {
    reaction = route.request().postDataJSON();
    await route.fulfill({ json: { ok: true } });
  });
  const message = page.locator('#messages-area .msg').last();
  const messageID = await message.getAttribute('data-msg-id');
  await message.hover();
  await message.locator('.action-react').click();
  await message.locator('.emoji-plus').click();
  const panel = message.locator('.emoji-full-panel');
  await panel.locator('input').fill('facepalm');
  const choice = panel.locator('button[data-emoji="🤦🏽‍♂️"]');
  await expect(choice).toHaveAttribute('aria-label', /man facepalming: medium skin tone/);
  await choice.click();
  await expect.poll(() => reaction).toMatchObject({
    message_id: messageID,
    emoji: '🤦🏽‍♂️',
    action: 'add',
  });
  await expect(panel).not.toHaveClass(/show/);
});

test.describe('offline emoji catalog', () => {
  test.use({ serviceWorkers: 'allow' });

  test('loads the bundled catalog after an offline reload', async ({ page, context }) => {
    await page.goto('/');
    await page.evaluate(() => navigator.serviceWorker.ready);
    await expect.poll(() => page.evaluate(async () => {
      const response = await caches.match('/emoji-data.js');
      return !!response && response.ok;
    })).toBe(true);
    await context.setOffline(true);
    await page.reload();
    expect(await page.evaluate(() => OPENMESSAGE_EMOJI_DATA.flatMap(category => category.emojis)
      .some(([emoji, name]) => emoji === '🤦' && name === 'person facepalming'))).toBe(true);
  });
});
