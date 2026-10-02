import test from 'node:test'
import assert from 'node:assert/strict'
import { completePrismOAuth, establishPrismSession, openPrismOAuthPopup, prismSessionAuthenticated, prismSessionIdentity } from './server.mjs'

const authenticatedSession = {
  status: 200,
  payload: { session: { auth_state: 'authenticated' }, user: { id: 'user-1' }, userTier: 'pro' }
}
const anonymousSession = { status: 200, payload: { session: null, user: null, userTier: 'logged_out' } }

function oauthPage(directVisible) {
  const events = []
  const popup = { name: 'oauth-popup' }
  const locator = (name, visible = true) => ({
    first() { return this },
    async isVisible() { return visible },
    async waitFor() { events.push(`wait:${name}`) },
    async click() { events.push(`click:${name}`) }
  })
  const page = {
    getByRole(role, { name }) {
      if (role === 'button' && name.test('Sign In or Sign Up')) return locator('direct', directVisible)
      if (role === 'button' && name.test('Continue with OpenAI')) return locator('continue')
      if (role === 'menuitem' && name.test('Sign in')) return locator('menuitem')
      throw new Error(`unexpected role: ${role}`)
    },
    locator(selector) {
      assert.equal(selector, '#status-bar-sign-in-menu-trigger')
      return locator('trigger')
    },
    waitForEvent(name) {
      assert.equal(name, 'popup')
      events.push('listen:popup')
      return Promise.resolve(popup)
    }
  }
  return { page, events, popup }
}

test('opens Prism OAuth through the desktop status menu', async () => {
  const { page, events, popup } = oauthPage(false)
  assert.equal(await openPrismOAuthPopup(page), popup)
  assert.deepEqual(events, [
    'wait:trigger', 'click:trigger', 'click:menuitem',
    'wait:continue', 'listen:popup', 'click:continue'
  ])
})

test('opens Prism OAuth through the mobile direct button', async () => {
  const { page, events, popup } = oauthPage(true)
  assert.equal(await openPrismOAuthPopup(page), popup)
  assert.deepEqual(events, [
    'click:direct', 'wait:continue', 'listen:popup', 'click:continue'
  ])
})

test('does not open OAuth when Prism already has an authenticated session', async () => {
  const page = { async evaluate() { return authenticatedSession } }
  assert.deepEqual(await establishPrismSession(page, {}), {
    prism_session_user_id: 'user-1', prism_session_email: ''
  })
})

test('returns when Prism authenticates without resubmitting the same OTP form', async () => {
  let probes = 0
  let fills = 0
  let clicks = 0
  const page = {
    async evaluate() { return ++probes < 4 ? anonymousSession : authenticatedSession },
    async waitForTimeout() {}
  }
  const popup = {
    isClosed() { return false },
    locator(selector) {
      return {
        first() { return this },
        async isVisible() { return selector === 'input[inputmode="numeric"]' },
        async fill() { fills++ }
      }
    },
    getByRole(role, { name }) {
      assert.equal(role, 'button')
      return {
        first() { return this },
        async isVisible() { return name.test('Continue') },
        async click() { clicks++ }
      }
    }
  }
  assert.deepEqual(await completePrismOAuth(page, popup, { mfa_secret: 'JBSWY3DPEHPK3PXP' }), {
    prism_session_user_id: 'user-1', prism_session_email: ''
  })
  assert.equal(probes, 4)
  assert.equal(fills, 1)
  assert.equal(clicks, 1)
})

test('rejects Prism anonymous sessions even when cookies exist', () => {
  assert.equal(prismSessionAuthenticated(200, {
    session: null,
    user: null,
    userTier: 'logged_out',
    resolutionDiagnostics: { reason: 'no_session_credentials' }
  }), false)
  assert.equal(prismSessionAuthenticated(200, {
    session: { auth_state: 'anonymous' },
    user: { id: 'user-1' },
    userTier: 'free'
  }), false)
  assert.equal(prismSessionAuthenticated(200, {
    session: { auth_state: 'authenticated' },
    user: { id: 'user-1', is_anonymous: true },
    userTier: 'free'
  }), false)
})

test('requires a successful response and an identified user', () => {
  assert.equal(prismSessionAuthenticated(503, { session: {}, user: { id: 'user-1' } }), false)
  assert.equal(prismSessionAuthenticated(200, { session: {}, user: {} }), false)
  assert.equal(prismSessionAuthenticated(200, { session: {}, user: null }), false)
})

test('accepts a non-anonymous session with an identified user', () => {
  assert.equal(prismSessionAuthenticated(200, {
    session: { auth_state: 'authenticated' },
    user: { id: 'user-1' },
    userTier: 'pro'
  }), true)
  assert.equal(prismSessionAuthenticated(200, {
    session: { expires: 'later' },
    user: { email: 'example@example.com' }
  }), true)
})

test('returns identity only from the authenticated Prism session user', () => {
  assert.deepEqual(prismSessionIdentity(200, {
    session: { auth_state: 'authenticated' },
    user: { openai_user_id: '  user-1  ', user_id: 'other-id', id: 'prism-id', email: '  a@example.com  ' },
    userTier: 'pro'
  }), { prism_session_user_id: 'user-1', prism_session_email: 'a@example.com' })
  assert.deepEqual(prismSessionIdentity(200, {
    session: { auth_state: 'authenticated' },
    user: { id: 'prism-id' },
    userTier: 'pro'
  }), { prism_session_user_id: 'prism-id', prism_session_email: '' })
  assert.equal(prismSessionIdentity(200, {
    session: { auth_state: 'anonymous' },
    user: { id: 'prism-id', email: 'a@example.com' },
    userTier: 'pro'
  }), null)
})
