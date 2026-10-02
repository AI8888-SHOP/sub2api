import http from 'node:http'
import crypto from 'node:crypto'
import { pathToFileURL } from 'node:url'
import { chromium } from 'playwright'

const port = Number(process.env.PORT || 8090)
const workerToken = String(process.env.PRISM_LOGIN_WORKER_TOKEN || '').trim()
const loginURL = process.env.OPENAI_LOGIN_URL || 'https://chatgpt.com/auth/login'
const prismURL = process.env.PRISM_URL || 'https://prism.openai.com'
const timeoutMs = Math.min(Math.max(Number(process.env.LOGIN_TIMEOUT_MS || 900000), 60000), 1800000)

function json(res, status, value) {
  const body = JSON.stringify(value)
  res.writeHead(status, { 'content-type': 'application/json', 'cache-control': 'no-store' })
  res.end(body)
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    let data = ''
    req.on('data', chunk => {
      data += chunk
      if (data.length > 128 * 1024) reject(new Error('request_too_large'))
    })
    req.on('end', () => resolve(JSON.parse(data || '{}')))
    req.on('error', reject)
  })
}

function secureEqual(a, b) {
  const left = Buffer.from(String(a))
  const right = Buffer.from(String(b))
  return left.length === right.length && crypto.timingSafeEqual(left, right)
}

function base32Decode(value) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
  const normalized = String(value).replace(/\s+/g, '').replace(/=+$/, '').toUpperCase()
  let bits = ''
  for (const char of normalized) {
    const index = alphabet.indexOf(char)
    if (index < 0) throw new Error('invalid_mfa_secret')
    bits += index.toString(2).padStart(5, '0')
  }
  const bytes = []
  for (let i = 0; i + 8 <= bits.length; i += 8) bytes.push(parseInt(bits.slice(i, i + 8), 2))
  return Buffer.from(bytes)
}

function totp(secret) {
  const key = base32Decode(secret)
  const counter = Math.floor(Date.now() / 1000 / 30)
  const message = Buffer.alloc(8)
  message.writeBigUInt64BE(BigInt(counter))
  const digest = crypto.createHmac('sha1', key).update(message).digest()
  const offset = digest[digest.length - 1] & 0xf
  const code = ((digest[offset] & 0x7f) << 24) | (digest[offset + 1] << 16) | (digest[offset + 2] << 8) | digest[offset + 3]
  return String(code % 1000000).padStart(6, '0')
}

const selectors = {
  email: ['input[type="email"]', 'input[name="email"]', 'input[autocomplete="username"]'],
  password: ['input[type="password"]', 'input[name="password"]', 'input[autocomplete="current-password"]'],
  otp: ['input[inputmode="numeric"]', 'input[name="code"]', 'input[autocomplete="one-time-code"]', 'input[type="tel"]']
}

async function firstVisible(page, candidates) {
  for (const selector of candidates) {
    const locator = page.locator(selector).first()
    if (await locator.isVisible().catch(() => false)) return locator
  }
  return null
}

async function fillIfPresent(page, candidates, value) {
  const locator = await firstVisible(page, candidates)
  if (!locator) return false
  await locator.fill(value)
  return true
}

async function clickContinue(page) {
  for (const text of ['Continue', '继续', 'Log in', '登录', 'Next', '下一步']) {
    const button = page.getByRole('button', { name: new RegExp(`^${text}$`, 'i') }).first()
    if (await button.isVisible().catch(() => false)) {
      await button.click()
      return true
    }
  }
  return false
}

export async function openPrismOAuthPopup(page) {
  const direct = page.getByRole('button', { name: /^Sign In or Sign Up$/i }).first()
  if (await direct.isVisible().catch(() => false)) {
    await direct.click()
  } else {
    const trigger = page.locator('#status-bar-sign-in-menu-trigger')
    await trigger.waitFor({ state: 'visible', timeout: 15000 })
    await trigger.click()
    await page.getByRole('menuitem', { name: /^Sign in$/i }).click()
  }
  const continueButton = page.getByRole('button', { name: /^Continue with OpenAI$/i })
  await continueButton.waitFor({ state: 'visible', timeout: 15000 })
  const popupPromise = page.waitForEvent('popup', { timeout: 30000 })
  await continueButton.click()
  return popupPromise
}

function walk(value, found) {
  if (!value || typeof value !== 'object') return
  for (const [key, child] of Object.entries(value)) {
    const lower = key.toLowerCase()
    if (typeof child === 'string') {
      if (!found.projectId && ['projectid', 'project_id', 'workspaceid', 'workspace_id'].includes(lower)) found.projectId = child
      if (!found.userId && ['userid', 'user_id'].includes(lower)) found.userId = child
      if (!found.sandboxURL && ['sandbox_url', 'sandboxurl', 'sandboxurl'].includes(lower)) found.sandboxURL = child
      if (!found.sandboxToken && ['sandbox_token', 'sandboxtoken'].includes(lower)) found.sandboxToken = child
    }
    walk(child, found)
  }
}

function idTokenClaims(token) {
  try {
    const part = String(token).split('.')[1]
    return JSON.parse(Buffer.from(part, 'base64url').toString('utf8'))
  } catch {
    return {}
  }
}

export function prismSessionAuthenticated(status, payload) {
  if (status !== 200 || !payload || typeof payload !== 'object') return false
  if (payload.userTier === 'logged_out' || payload.userTier === 'anonymous') return false
  const session = payload.session
  const user = payload.user
  if (!session || !user || typeof user !== 'object' || Array.isArray(user)) return false
  if (session?.auth_state === 'anonymous' || session?.is_anonymous === true) return false
  if (user.is_anonymous === true) return false
  return ['id', 'user_id', 'openai_user_id', 'email'].some(key =>
    typeof user[key] === 'string' && user[key].trim() !== '')
}

export function prismSessionIdentity(status, payload) {
  if (!prismSessionAuthenticated(status, payload)) return null
  const user = payload.user
  const text = value => typeof value === 'string' ? value.trim() : ''
  return {
    prism_session_user_id: text(user.openai_user_id) || text(user.user_id) || text(user.id),
    prism_session_email: text(user.email)
  }
}

async function readPrismSession(page) {
  try {
    const result = await page.evaluate(async () => {
      const response = await fetch('/auth/session', { credentials: 'include' })
      return { status: response.status, payload: await response.json() }
    })
    return prismSessionIdentity(result.status, result.payload)
  } catch {
    // The page may still be navigating while Prism establishes its session.
    return null
  }
}

export async function completePrismOAuth(page, popup, input, timeout = 60000) {
  const deadline = Date.now() + Math.min(timeout, 60000)
  let submittedStep = ''
  while (Date.now() < deadline) {
    const identity = await readPrismSession(page)
    if (identity) return identity
    if (!popup.isClosed()) {
      for (const [step, candidates, value] of [
        ['email', selectors.email, input.email],
        ['password', selectors.password, input.password],
        ['otp', selectors.otp, null]
      ]) {
        const field = await firstVisible(popup, candidates)
        if (!field) continue
        if (submittedStep !== step) {
          await field.fill(step === 'otp' ? totp(input.mfa_secret) : value)
          if (await clickContinue(popup)) submittedStep = step
        }
        break
      }
    }
    await page.waitForTimeout(1000)
  }
  throw new Error('prism_auth_required')
}

export async function establishPrismSession(page, input, timeout = 60000) {
  const identity = await readPrismSession(page)
  if (identity) return identity
  const popup = await openPrismOAuthPopup(page)
  return completePrismOAuth(page, popup, input, timeout)
}

async function capture(input) {
  const browser = await chromium.launch({ headless: process.env.HEADLESS !== 'false' })
  const context = await browser.newContext({ locale: 'en-US' })
  const observed = {}
  const page = await context.newPage()
  page.on('response', async response => {
    const type = response.headers()['content-type'] || ''
    if (!type.includes('json')) return
    try {
      const length = Number(response.headers()['content-length'] || 0)
      if (length > 2 * 1024 * 1024) return
      walk(await response.json(), observed)
    } catch {
      // Some streaming endpoints are not JSON despite their content type.
    }
  })
  try {
    await page.goto(loginURL, { waitUntil: 'domcontentloaded', timeout: timeoutMs })
    await fillIfPresent(page, selectors.email, input.email)
    await clickContinue(page)
    await page.waitForTimeout(800)
    await fillIfPresent(page, selectors.password, input.password)
    await clickContinue(page)
    await page.waitForTimeout(1000)
    const code = totp(input.mfa_secret)
    await fillIfPresent(page, selectors.otp, code)
    await clickContinue(page)
    await page.waitForLoadState('domcontentloaded', { timeout: timeoutMs }).catch(() => {})
    await page.waitForTimeout(1500)
    if (await firstVisible(page, selectors.password)) throw new Error('openai_login_failed')
    await page.goto(prismURL, { waitUntil: 'networkidle', timeout: timeoutMs })
    await page.waitForTimeout(1500)
    const sessionIdentity = await establishPrismSession(page, input, timeoutMs)
    // Only export cookies that the Prism origin itself would send. The login
    // context also contains ChatGPT/OpenAI cookies, but flattening those into
    // one Cookie header would leak cross-site credentials when the adapter
    // seeds a Prism browser context.
    const cookies = await context.cookies([prismURL])
    const cookieHeader = cookies.map(item => `${item.name}=${item.value}`).join('; ')
    if (!cookies.some(item => item.name === 'prism_session_token')) throw new Error('prism_cookie_not_found')
    const claims = idTokenClaims(input.id_token)
    const projectId = observed.projectId || claims['https://api.openai.com/auth']?.chatgpt_account_id || claims.chatgpt_account_id
    const userId = observed.userId || claims['https://api.openai.com/auth']?.chatgpt_user_id || claims.sub
    const sandboxToken = observed.sandboxToken || input.access_token
    const sandboxURL = observed.sandboxURL || prismURL
    if (!projectId || !userId || !sandboxToken) throw new Error('prism_metadata_not_found')
    return {
      ...sessionIdentity,
      prism_cookie: cookieHeader,
      prism_project_id: projectId,
      prism_user_id: userId,
      prism_sandbox_url: sandboxURL,
      prism_sandbox_token: sandboxToken,
      prism_template: {
        metadata: { projectId, userId, sandbox_url: sandboxURL, sandbox_token: sandboxToken },
        headers: { Cookie: cookieHeader, ...(input.access_token ? { Authorization: `Bearer ${input.access_token}` } : {}) }
      }
    }
  } finally {
    await context.close().catch(() => {})
    await browser.close().catch(() => {})
  }
}

const server = http.createServer(async (req, res) => {
  if (req.method === 'GET' && req.url === '/healthz') return json(res, 200, { ok: true, configured: Boolean(workerToken) })
  if (req.method !== 'POST' || req.url !== '/login') return json(res, 404, { error: { code: 'not_found' } })
  if (!workerToken) return json(res, 503, { error: { code: 'worker_not_configured' } })
  const auth = req.headers.authorization || ''
  if (!auth.startsWith('Bearer ') || !secureEqual(auth.slice(7), workerToken)) return json(res, 401, { error: { code: 'unauthorized' } })
  try {
    const input = await readBody(req)
    if (!input.email || !input.password || !input.mfa_secret) throw new Error('missing_login_fields')
    const prism = await capture(input)
    return json(res, 200, { prism })
  } catch (error) {
    return json(res, 422, { error: { code: 'prism_login_failed', message: String(error?.message || 'login_failed') } })
  }
})

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  server.listen(port, '0.0.0.0')
}
