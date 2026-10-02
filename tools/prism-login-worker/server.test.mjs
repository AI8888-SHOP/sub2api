import test from 'node:test'
import assert from 'node:assert/strict'
import { prismSessionAuthenticated, prismSessionIdentity } from './server.mjs'

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
