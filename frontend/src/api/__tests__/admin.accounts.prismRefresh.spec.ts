import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get, post } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))

vi.mock('@/api/client', () => ({ apiClient: { get, post } }))

import { getPrismRefresh, startPrismRefresh } from '@/api/admin/accounts'

describe('admin Prism credential refresh API', () => {
  beforeEach(() => {
    get.mockReset()
    post.mockReset()
  })

  it('starts with saved server-side credentials and polls the returned job', async () => {
    const running = { id: 'job-1', account_id: 7, status: 'running' }
    const succeeded = { ...running, status: 'succeeded' }
    post.mockResolvedValueOnce({ data: running })
    get.mockResolvedValueOnce({ data: succeeded })

    await expect(startPrismRefresh(7)).resolves.toEqual(running)
    await expect(getPrismRefresh(7, 'job-1')).resolves.toEqual(succeeded)
    expect(post).toHaveBeenCalledWith('/admin/accounts/7/prism-refresh')
    expect(get).toHaveBeenCalledWith('/admin/accounts/7/prism-refresh/job-1')
  })
})
